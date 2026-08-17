package inventory

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows sessions come from the Terminal Services API rather than gopsutil,
// whose Users() is ErrNotImplementedError on this platform. WTS is the better
// source anyway: it distinguishes the physical console from an RDP session and
// reports disconnected sessions, neither of which any utmp-shaped API has a
// concept of.
var (
	wtsapi32                       = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSEnumerateSessionsW      = wtsapi32.NewProc("WTSEnumerateSessionsW")
	procWTSQuerySessionInformation = wtsapi32.NewProc("WTSQuerySessionInformationW")
	procWTSFreeMemory              = wtsapi32.NewProc("WTSFreeMemory")
)

// WTS_CONNECTSTATE_CLASS values we care to name.
const (
	wtsActive       = 0
	wtsConnected    = 1
	wtsConnectQuery = 2
	wtsShadow       = 3
	wtsDisconnected = 4
	wtsIdle         = 5
	wtsListen       = 6
	wtsReset        = 7
	wtsDown         = 8
	wtsInit         = 9
)

// WTS_INFO_CLASS values.
const (
	wtsUserName    = 5
	wtsDomainName  = 7
	wtsClientName  = 10
	wtsSessionInfo = 24
)

type wtsSessionInfoW struct {
	SessionID      uint32
	WinStationName *uint16
	State          uint32
}

// wtsInfoW mirrors WTSINFOW. The fixed-size name arrays are the documented
// lengths: WINSTATIONNAME_LENGTH 32, DOMAIN_LENGTH 17, USERNAME_LENGTH 20.
//
// Hand-mirrored C structs are the classic way to read plausible garbage, so
// SessionID is used as a layout checksum below rather than trusted blindly.
type wtsInfoW struct {
	State                   uint32
	SessionID               uint32
	IncomingBytes           uint32
	OutgoingBytes           uint32
	IncomingFrames          uint32
	OutgoingFrames          uint32
	IncomingCompressedBytes uint32
	OutgoingCompressedBytes uint32
	WinStationName          [32]uint16
	Domain                  [17]uint16
	UserName                [21]uint16
	// Go inserts the same padding the C compiler does here, because the
	// int64s below force 8-byte alignment after the odd-length name arrays.
	ConnectTime    int64
	DisconnectTime int64
	LastInputTime  int64
	LogonTime      int64
	CurrentTime    int64
}

func currentLogins(ctx context.Context) ([]Login, error) {
	var (
		sessions *wtsSessionInfoW
		count    uint32
	)
	// WTS_CURRENT_SERVER_HANDLE is 0: this machine.
	r, _, err := procWTSEnumerateSessionsW.Call(
		0, 0, 1,
		uintptr(unsafe.Pointer(&sessions)),
		uintptr(unsafe.Pointer(&count)),
	)
	if r == 0 {
		return nil, err
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(sessions)))

	list := unsafe.Slice(sessions, count)
	logins := make([]Login, 0, count)
	for _, s := range list {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Session 0 is the non-interactive services session and Listen is the
		// RDP listener itself. Neither is a person, and reporting them as
		// logged-in users is how a fleet ends up looking permanently occupied.
		if s.SessionID == 0 || s.State == wtsListen {
			continue
		}

		user, err := queryString(s.SessionID, wtsUserName)
		if err != nil {
			// Fail the whole collector rather than skipping this session.
			// Skipping would silently understate who is logged in, and the
			// server records anything missing from a snapshot as ended.
			return nil, fmt.Errorf("logins: session %d user name: %w", s.SessionID, err)
		}
		if user == "" {
			// An unoccupied console session still enumerates, with no user.
			continue
		}
		domain, err := queryString(s.SessionID, wtsDomainName)
		if err != nil {
			return nil, fmt.Errorf("logins: session %d domain: %w", s.SessionID, err)
		}
		if domain != "" {
			user = domain + `\` + user
		}

		station := windows.UTF16PtrToString(s.WinStationName)
		// WTSClientName is the name of the machine an RDP client connected
		// from, and is empty for a console session.
		host, err := queryString(s.SessionID, wtsClientName)
		if err != nil {
			return nil, fmt.Errorf("logins: session %d client name: %w", s.SessionID, err)
		}

		logins = append(logins, Login{
			User:     user,
			Terminal: station,
			Host:     host,
			Kind:     classifyWindows(station, host),
			State:    stateName(s.State),
			Since:    sinceString(logonTime(s.SessionID)),
		})
	}
	return logins, nil
}

// classifyWindows names the seat.
//
// The station name is authoritative here, unlike on Unix: Windows calls the
// physical console exactly "Console" and every remote session "RDP-Tcp#n". The
// shared classify() would get this right too, but only by accident of its
// prefix list, and this states the rule where it is actually known.
func classifyWindows(station, host string) string {
	if strings.EqualFold(station, "Console") && host == "" {
		return kindConsole
	}
	return kindRemote
}

func stateName(state uint32) string {
	switch state {
	case wtsActive:
		return "active"
	case wtsConnected:
		return "connected"
	case wtsConnectQuery:
		return "connecting"
	case wtsShadow:
		return "shadowing"
	case wtsDisconnected:
		// The session is still alive with the user's programs running. Very
		// much not the same as logged out, and the usual reason a "logged in"
		// count looks too high to somebody expecting Unix semantics.
		return "disconnected"
	case wtsIdle:
		return "idle"
	case wtsReset:
		return "reset"
	case wtsDown:
		return "down"
	case wtsInit:
		return "init"
	default:
		return ""
	}
}

// queryString reads one string-valued session property.
//
// Returns an error rather than an empty string when the query fails. This
// distinction is the whole collector: the caller treats an empty user name as
// "an unoccupied console session" and skips the row, so an agent that is not
// LocalSystem, where WTSQuerySessionInformationW returns ERROR_ACCESS_DENIED
// for every session it does not own, would skip every session and publish
// "nobody is logged in" as fact. That is a confident wrong answer to the exact
// question this collector exists to answer.
func queryString(sessionID uint32, class uint32) (string, error) {
	var (
		buf   *uint16
		bytes uint32
	)
	r, _, callErr := procWTSQuerySessionInformation.Call(
		0,
		uintptr(sessionID),
		uintptr(class),
		uintptr(unsafe.Pointer(&buf)),
		uintptr(unsafe.Pointer(&bytes)),
	)
	if r == 0 {
		if callErr == nil {
			callErr = windows.ERROR_INVALID_FUNCTION
		}
		return "", callErr
	}
	if buf == nil {
		// Succeeded with no value. A genuinely absent property, which is
		// normal: WTSClientName is empty for a console session.
		return "", nil
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(buf)))
	return strings.TrimSpace(windows.UTF16PtrToString(buf)), nil
}

// logonTime reads when the session started, or the zero time if it cannot be
// read or the struct did not come back the shape we expect.
func logonTime(sessionID uint32) time.Time {
	var (
		buf   *wtsInfoW
		bytes uint32
	)
	r, _, _ := procWTSQuerySessionInformation.Call(
		0,
		uintptr(sessionID),
		uintptr(wtsSessionInfo),
		uintptr(unsafe.Pointer(&buf)),
		uintptr(unsafe.Pointer(&bytes)),
	)
	if r == 0 || buf == nil {
		return time.Time{}
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(buf)))

	if bytes < uint32(unsafe.Sizeof(*buf)) {
		return time.Time{}
	}
	// Layout checksum. The struct echoes back the session id we asked about,
	// so if our field offsets are wrong this will not match and we report no
	// timestamp instead of interpreting whatever bytes happen to sit at the
	// offset we guessed. A wrong FILETIME here is not obviously wrong: it is a
	// plausible-looking date that would quietly become the login time.
	if buf.SessionID != sessionID {
		return time.Time{}
	}
	if buf.LogonTime <= 0 {
		return time.Time{}
	}
	ft := windows.Filetime{
		LowDateTime:  uint32(buf.LogonTime & 0xFFFFFFFF),
		HighDateTime: uint32(uint64(buf.LogonTime) >> 32),
	}
	return time.Unix(0, ft.Nanoseconds())
}
