package scripts

import "os"

// outPipes is the parent's end of a child's stdout and stderr.
//
// exec.Cmd's own StdoutPipe would be less code, but Wait closes those pipes
// as soon as the process exits, so a reader still draining loses whatever
// was left in the buffer. Owning the files puts the "when do we stop
// reading" decision where the timeout policy already lives.
type outPipes struct {
	outR, outW *os.File
	errR, errW *os.File
}

func newOutPipes() (*outPipes, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, err
	}
	return &outPipes{outR: outR, outW: outW, errR: errR, errW: errW}, nil
}

// closeWrite drops this process's copies of the write ends, which the child
// inherited. Until they are gone the reads cannot see EOF.
func (p *outPipes) closeWrite() {
	_ = p.outW.Close()
	_ = p.errW.Close()
}

// closeRead unblocks the readers even though something still holds the write
// end. On Windows a blocked read may not return; the caller must not wait on
// the reader goroutines after calling this.
func (p *outPipes) closeRead() {
	_ = p.outR.Close()
	_ = p.errR.Close()
}

// closeAll is the cleanup path; closing an already closed file is harmless.
func (p *outPipes) closeAll() {
	p.closeWrite()
	p.closeRead()
}
