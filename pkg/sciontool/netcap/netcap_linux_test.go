//go:build linux

/*
Copyright 2026 The Scion Authors.
*/

package netcap

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
)

// status builds a /proc status body. bnd/eff/prm etc. are hex strings.
func status(inh, prm, eff, bnd, amb string) string {
	return fmt.Sprintf("Name:\tsciontool\nCapInh:\t%s\nCapPrm:\t%s\nCapEff:\t%s\nCapBnd:\t%s\nCapAmb:\t%s\n", inh, prm, eff, bnd, amb)
}

const (
	// Docker default caps plus NET_ADMIN (bit 12 = 0x1000) and SETPCAP.
	withNetAdmin    = "00000000a80435fb"
	withoutNetAdmin = "00000000a80425fb"
	zero            = "0000000000000000"
)

var (
	heldStatus  = status(zero, withNetAdmin, withNetAdmin, withNetAdmin, zero)
	clearStatus = status(zero, withoutNetAdmin, withoutNetAdmin, withoutNetAdmin, zero)
)

type call struct {
	trap, a1, a2, a3 uintptr
	capsetData       [2]capData
}

type fake struct {
	calls     []call
	statuses  [][]string // returned by successive readThreadStatuses calls
	readErr   error
	failOn    func(c call) syscall.Errno
	capgetErr error
}

func (f *fake) record(c call) syscall.Errno {
	f.calls = append(f.calls, c)
	if f.failOn != nil {
		return f.failOn(c)
	}
	return 0
}

func install(t *testing.T, f *fake) {
	t.Helper()
	origSys, origGet, origSet, origRead := allThreadsSyscall6, capget, capsetAllThreads, readThreadStatuses
	t.Cleanup(func() {
		allThreadsSyscall6, capget, capsetAllThreads, readThreadStatuses = origSys, origGet, origSet, origRead
	})

	allThreadsSyscall6 = func(trap, a1, a2, a3, a4, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno) {
		return 0, 0, f.record(call{trap: trap, a1: a1, a2: a2, a3: a3})
	}
	capsetAllThreads = func(hdr *capHeader, data *[2]capData) syscall.Errno {
		if hdr.version != linuxCapabilityVersion3 || hdr.pid != 0 {
			t.Errorf("capset header = %+v", *hdr)
		}
		return f.record(call{trap: syscall.SYS_CAPSET, capsetData: *data})
	}
	capget = func(hdr *capHeader, data *[2]capData) error {
		if f.capgetErr != nil {
			return f.capgetErr
		}
		if hdr.version != linuxCapabilityVersion3 {
			t.Errorf("capget version = %#x", hdr.version)
		}
		v := uint32(0xa80435fb)
		data[0] = capData{effective: v, permitted: v, inheritable: 1 << capNetAdmin}
		return nil
	}
	readThreadStatuses = func() ([]string, error) {
		if f.readErr != nil {
			return nil, f.readErr
		}
		if len(f.statuses) == 0 {
			t.Fatal("unexpected readThreadStatuses call")
		}
		s := f.statuses[0]
		f.statuses = f.statuses[1:]
		return s, nil
	}
}

func isAmbient(c call) bool  { return c.trap == syscall.SYS_PRCTL && c.a1 == prCapAmbient }
func isBounding(c call) bool { return c.trap == syscall.SYS_PRCTL && c.a1 == prCapbsetDrop }
func isCapset(c call) bool   { return c.trap == syscall.SYS_CAPSET }

// checkErrText enforces that errors carry only the capability name and step.
func checkErrText(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), CapabilityName) {
		t.Errorf("error %q does not name %s", err, CapabilityName)
	}
	if strings.Contains(err.Error(), "=") || strings.Contains(err.Error(), "/") {
		t.Errorf("error %q carries more than the capability and step", err)
	}
}

func TestDropNetAdmin_NoopWhenAbsent(t *testing.T) {
	// A capability-less start (gVisor, Cloud Run, restricted pods): no
	// prctl, no capget, no capset — and success.
	f := &fake{statuses: [][]string{{clearStatus, clearStatus}}}
	install(t, f)
	f.capgetErr = errors.New("capget must not be called")
	if err := DropNetAdmin(); err != nil {
		t.Fatalf("DropNetAdmin() = %v, want nil", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("made %d syscalls on a start without %s", len(f.calls), CapabilityName)
	}
}

func TestDropNetAdmin_BoundingOnlyStillDrops(t *testing.T) {
	// Held only in the bounding set: a later setuid-root exec (sudo) would
	// regain it, so it counts as held.
	f := &fake{statuses: [][]string{
		{status(zero, withoutNetAdmin, withoutNetAdmin, withNetAdmin, zero)},
		{clearStatus},
	}}
	install(t, f)
	if err := DropNetAdmin(); err != nil {
		t.Fatalf("DropNetAdmin() = %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(f.calls))
	}
}

func TestDropNetAdmin_Sequence(t *testing.T) {
	f := &fake{statuses: [][]string{{heldStatus, heldStatus}, {clearStatus, clearStatus}}}
	install(t, f)
	if err := DropNetAdmin(); err != nil {
		t.Fatalf("DropNetAdmin() = %v", err)
	}
	if len(f.calls) != 3 || !isAmbient(f.calls[0]) || !isBounding(f.calls[1]) || !isCapset(f.calls[2]) {
		t.Fatalf("calls = %+v, want ambient, bounding, capset", f.calls)
	}
	if f.calls[0].a2 != prCapAmbientLower {
		t.Errorf("ambient op = %d, want LOWER", f.calls[0].a2)
	}
	if f.calls[0].a3 != capNetAdmin {
		t.Errorf("ambient cap arg = %d, want %d", f.calls[0].a3, capNetAdmin)
	}
	if f.calls[1].a2 != capNetAdmin {
		t.Errorf("bounding cap arg = %d, want %d", f.calls[1].a2, capNetAdmin)
	}
	d := f.calls[2].capsetData[0]
	bit := uint32(1 << capNetAdmin)
	if d.effective&bit != 0 || d.permitted&bit != 0 || d.inheritable&bit != 0 {
		t.Errorf("capset left bit 12 set: %+v", d)
	}
	if d.effective != 0xa80435fb&^bit || d.permitted != 0xa80435fb&^bit {
		t.Errorf("capset changed other bits: %+v", d)
	}
}

func TestDropNetAdmin_StepFailures(t *testing.T) {
	tests := []struct {
		name   string
		failOn func(c call) syscall.Errno
		ok     bool
	}{
		{"ambient EPERM", func(c call) syscall.Errno {
			if isAmbient(c) {
				return syscall.EPERM
			}
			return 0
		}, false},
		{"ambient EINVAL tolerated (pre-4.3 kernel)", func(c call) syscall.Errno {
			if isAmbient(c) {
				return syscall.EINVAL
			}
			return 0
		}, true},
		{"bounding EPERM (no CAP_SETPCAP)", func(c call) syscall.Errno {
			if isBounding(c) {
				return syscall.EPERM
			}
			return 0
		}, false},
		{"ENOTSUP (cgo linked in)", func(c call) syscall.Errno { return syscall.ENOTSUP }, false},
		{"capset EPERM", func(c call) syscall.Errno {
			if isCapset(c) {
				return syscall.EPERM
			}
			return 0
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fake{statuses: [][]string{{heldStatus}, {clearStatus}}, failOn: tt.failOn}
			install(t, f)
			err := DropNetAdmin()
			if tt.ok {
				if err != nil {
					t.Fatalf("DropNetAdmin() = %v, want nil", err)
				}
				return
			}
			checkErrText(t, err)
		})
	}
}

func TestDropNetAdmin_CapgetFailure(t *testing.T) {
	f := &fake{statuses: [][]string{{heldStatus}}, capgetErr: syscall.EFAULT}
	install(t, f)
	checkErrText(t, DropNetAdmin())
}

func TestDropNetAdmin_VerifyCatchesOneThread(t *testing.T) {
	// Every syscall "succeeds" but one of four threads still holds the bit
	// in its bounding set: the drop must fail closed.
	stuck := status(zero, withoutNetAdmin, withoutNetAdmin, withNetAdmin, zero)
	f := &fake{statuses: [][]string{
		{heldStatus, heldStatus, heldStatus, heldStatus},
		{clearStatus, clearStatus, stuck, clearStatus},
	}}
	install(t, f)
	err := DropNetAdmin()
	if !errors.Is(err, ErrStillHeld) {
		t.Fatalf("DropNetAdmin() = %v, want ErrStillHeld", err)
	}
	checkErrText(t, err)
}

func TestDropNetAdmin_VerifyCatchesAmbient(t *testing.T) {
	f := &fake{statuses: [][]string{{heldStatus}, {status(zero, withoutNetAdmin, withoutNetAdmin, withoutNetAdmin, "0000000000001000")}}}
	install(t, f)
	if err := DropNetAdmin(); !errors.Is(err, ErrStillHeld) {
		t.Fatalf("DropNetAdmin() = %v, want ErrStillHeld", err)
	}
}

func TestDropNetAdmin_UnreadableStatusFailsClosed(t *testing.T) {
	f := &fake{readErr: syscall.EACCES}
	install(t, f)
	checkErrText(t, DropNetAdmin())

	f2 := &fake{statuses: [][]string{{"Name:\tx\nCapEff:\t0\n"}}}
	install(t, f2)
	checkErrText(t, DropNetAdmin())
}
