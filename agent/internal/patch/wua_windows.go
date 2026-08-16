package patch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

// wuaManager drives the Windows Update Agent over COM.
//
// Everything that touches COM runs on the single thread owned by comThread;
// the exported methods are thin wrappers that hand a closure over and wait.
// Two things this backend will never do: reboot the machine (it reports
// RebootRequired and stops), and install a driver (the search criteria is
// Type='Software').
type wuaManager struct {
	loggerHolder
	gate installGate

	once sync.Once
	com  *comThread
}

func newWUAManager() *wuaManager { return &wuaManager{} }

func (m *wuaManager) Kind() string { return BackendWUA }

// thread starts the COM thread on first use.
func (m *wuaManager) thread() *comThread {
	m.once.Do(func() { m.com = newCOMThread() })
	return m.com
}

// Scan runs a Windows Update search. A search routinely takes 2 to 10
// minutes on a machine that has not checked in for a while, most of it
// inside a single COM call that cannot be interrupted, so callers must not
// wrap this in a short timeout.
func (m *wuaManager) Scan(ctx context.Context) ([]Update, error) {
	updates := []Update{}
	err := m.thread().do(ctx, func() error {
		return withUpdateSearch(func(coll *ole.IDispatch, count int64) error {
			for i := int64(0); i < count; i++ {
				item, err := propDispatch(coll, "Item", int(i))
				if err != nil {
					m.logger().Warn("skipping unreadable update in search result",
						"index", i, "err", err)
					continue
				}
				updates = append(updates, wuaUpdateFromFields(readUpdateFields(item)))
				item.Release()
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return updates, nil
}

// Install downloads and installs the requested updates in one WUA
// transaction.
//
// Progress is coarse by design: the synchronous Download and Install calls
// report nothing until they return, and the alternative (implementing
// IDownloadProgressChangedCallback as a COM object) buys percentage ticks
// at the cost of a callback surface that can deadlock the apartment. Phase
// transitions are honest; sub-percentages would not be.
func (m *wuaManager) Install(ctx context.Context, ids []string, progress func(InstallProgress)) (InstallResult, error) {
	ids = dedupeIDs(ids)
	res := newInstallResult()
	if len(ids) == 0 {
		return res, nil
	}
	if !m.gate.acquire() {
		return res, ErrBusy
	}
	defer m.gate.release()

	err := m.thread().do(ctx, func() error {
		return m.install(ids, &res, progress)
	})
	return res, err
}

// install runs entirely on the COM thread.
func (m *wuaManager) install(ids []string, res *InstallResult, progress func(InstallProgress)) error {
	session, err := createObject("Microsoft.Update.Session")
	if err != nil {
		return err
	}
	defer session.Release()

	installer, err := callDispatch(session, "CreateUpdateInstaller")
	if err != nil {
		return err
	}
	defer installer.Release()

	// IsBusy means another installer (Windows Update in the UI, an MDM
	// agent, a servicing task) holds the update session. Returning ErrBusy
	// lets the server requeue instead of failing the job outright.
	if propBool(installer, "IsBusy") {
		return ErrBusy
	}

	batch, err := createObject("Microsoft.Update.UpdateColl")
	if err != nil {
		return err
	}
	defer batch.Release()

	var ordered []string // update ids in the order they went into the batch
	err = withUpdateSearch(func(coll *ole.IDispatch, count int64) error {
		wanted := make(map[string]bool, len(ids))
		for _, id := range ids {
			wanted[lowerTrim(id)] = true
		}
		for i := int64(0); i < count; i++ {
			item, err := propDispatch(coll, "Item", int(i))
			if err != nil {
				continue
			}
			fields := readUpdateFields(item)
			if !wanted[lowerTrim(fields.UpdateID)] {
				item.Release()
				continue
			}
			if fields.CanRequestUserInput {
				res.fail(fields.UpdateID, errors.New(detailUserInput))
				item.Release()
				continue
			}
			// An update with an unaccepted EULA is refused by the installer
			// with a bare HRESULT, so accept it here.
			if !fields.EulaAccepted {
				if _, err := oleutil.CallMethod(item, "AcceptEula"); err != nil {
					res.fail(fields.UpdateID, fmt.Errorf("accept eula: %w", err))
					item.Release()
					continue
				}
			}
			if _, err := oleutil.CallMethod(batch, "Add", item); err != nil {
				res.fail(fields.UpdateID, fmt.Errorf("add to batch: %w", err))
				item.Release()
				continue
			}
			ordered = append(ordered, fields.UpdateID)
			item.Release()
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Anything the search did not return is no longer offered: superseded,
	// already installed, or hidden since the scan.
	inBatch := make(map[string]bool, len(ordered))
	for _, id := range ordered {
		inBatch[lowerTrim(id)] = true
	}
	for _, id := range ids {
		if !inBatch[lowerTrim(id)] {
			if _, alreadyFailed := res.Failed[id]; !alreadyFailed {
				res.fail(id, errors.New("update is no longer offered by Windows Update"))
			}
		}
	}
	if len(ordered) == 0 {
		return nil
	}

	emitProgress(progress, InstallProgress{Phase: PhaseDownload, Pct: 10})
	if err := m.download(session, batch); err != nil {
		for _, id := range ordered {
			res.fail(id, err)
		}
		return err
	}

	emitProgress(progress, InstallProgress{Phase: PhaseInstall, Pct: 50})
	if _, err := oleutil.PutProperty(installer, "Updates", batch); err != nil {
		return fmt.Errorf("installer.Updates: %w", err)
	}
	installResult, err := callDispatch(installer, "Install")
	if err != nil {
		for _, id := range ordered {
			res.fail(id, err)
		}
		return fmt.Errorf("install: %w", err)
	}
	defer installResult.Release()

	res.RebootRequired = propBool(installResult, "RebootRequired")
	m.recordOutcomes(installResult, ordered, res, progress)
	return nil
}

// download fetches the batch. An update already in the cache downloads
// instantly, so this is cheap to call unconditionally.
func (m *wuaManager) download(session, batch *ole.IDispatch) error {
	downloader, err := callDispatch(session, "CreateUpdateDownloader")
	if err != nil {
		return err
	}
	defer downloader.Release()
	if _, err := oleutil.PutProperty(downloader, "Updates", batch); err != nil {
		return fmt.Errorf("downloader.Updates: %w", err)
	}
	result, err := callDispatch(downloader, "Download")
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer result.Release()
	if code := propInt64(result, "ResultCode"); !wuaResultOK(code) {
		return fmt.Errorf("download %s", wuaResultText(code, propInt64(result, "HResult")))
	}
	return nil
}

// recordOutcomes reads the per-update result codes out of the installation
// result. The batch order is the index order, which is why ordered is kept.
func (m *wuaManager) recordOutcomes(installResult *ole.IDispatch, ordered []string, res *InstallResult, progress func(InstallProgress)) {
	for i, id := range ordered {
		code := int64(wuaResultFailed)
		hresult := int64(0)
		if per, err := callDispatch(installResult, "GetUpdateResult", i); err == nil {
			code = propInt64(per, "ResultCode")
			hresult = propInt64(per, "HResult")
			per.Release()
		} else {
			// Fall back to the batch-level code so a missing per-update
			// result does not report every update as failed.
			code = propInt64(installResult, "ResultCode")
			hresult = propInt64(installResult, "HResult")
		}
		if wuaResultOK(code) {
			res.Installed = append(res.Installed, id)
		} else {
			res.fail(id, errors.New(wuaResultText(code, hresult)))
		}
		emitProgress(progress, InstallProgress{
			UpdateID: id, Phase: PhaseInstall, Pct: pctOf(i+1, len(ordered)),
		})
	}
}

// RebootRequired reads Microsoft.Update.SystemInfo. The agent reports it
// and never acts on it: rebooting a server because a patch asked for it is
// the operator's decision, made in their maintenance window.
func (m *wuaManager) RebootRequired(ctx context.Context) (bool, error) {
	var required bool
	err := m.thread().do(ctx, func() error {
		info, err := createObject("Microsoft.Update.SystemInfo")
		if err != nil {
			return err
		}
		defer info.Release()
		required = propBool(info, "RebootRequired")
		return nil
	})
	return required, err
}

// withUpdateSearch runs one search and hands the result collection to fn.
// It exists so Scan and Install cannot drift apart on search criteria or on
// releasing COM objects.
func withUpdateSearch(fn func(coll *ole.IDispatch, count int64) error) error {
	session, err := createObject("Microsoft.Update.Session")
	if err != nil {
		return err
	}
	defer session.Release()

	searcher, err := callDispatch(session, "CreateUpdateSearcher")
	if err != nil {
		return err
	}
	defer searcher.Release()

	result, err := callDispatch(searcher, "Search", wuaSearchCriteria)
	if err != nil {
		return fmt.Errorf("windows update search: %w", err)
	}
	defer result.Release()

	coll, err := propDispatch(result, "Updates")
	if err != nil {
		return err
	}
	defer coll.Release()

	return fn(coll, propInt64(coll, "Count"))
}

// readUpdateFields flattens one IUpdate. Every field is best effort: a
// property this Windows build does not expose comes back as a zero value
// rather than failing the scan.
func readUpdateFields(item *ole.IDispatch) wuaFields {
	f := wuaFields{
		Title:           propString(item, "Title"),
		MsrcSeverity:    propString(item, "MsrcSeverity"),
		MaxDownloadSize: propInt64(item, "MaxDownloadSize"),
		EulaAccepted:    propBool(item, "EulaAccepted"),
		Description:     propString(item, "Description"),
		KBIDs:           collectStrings(item, "KBArticleIDs"),
		Categories:      collectCategoryNames(item),
	}
	if identity, err := propDispatch(item, "Identity"); err == nil {
		f.UpdateID = propString(identity, "UpdateID")
		f.RevisionNumber = propInt64(identity, "RevisionNumber")
		identity.Release()
	}
	if behavior, err := propDispatch(item, "InstallationBehavior"); err == nil {
		f.RebootBehavior = propInt64(behavior, "RebootBehavior")
		f.CanRequestUserInput = propBool(behavior, "CanRequestUserInput")
		behavior.Release()
	}
	return f
}

func lowerTrim(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
