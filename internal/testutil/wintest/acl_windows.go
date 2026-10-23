// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows

package wintest

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Well-known SID strings used by the tests.
const (
	SIDSystem         = "S-1-5-18"
	SIDAdministrators = "S-1-5-32-544"
	SIDUsers          = "S-1-5-32-545"
	SIDEveryone       = "S-1-1-0"
)

// FileAllAccess is FILE_ALL_ACCESS ("FA" in SDDL).
const FileAllAccess = 0x1F01FF

// UserSID returns the SID string of the process token's user.
func UserSID(t testing.TB) string {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	return tu.User.Sid.String()
}

// TokenOwnerSID returns the SID string of the effective token's default
// owner (TokenOwner): the owner a new object gets when none is given. Inside
// WithDefaultOwner that is the impersonation token's.
func TokenOwnerSID(t testing.TB) string {
	t.Helper()
	tok := windows.GetCurrentThreadEffectiveToken()
	var n uint32
	_ = windows.GetTokenInformation(tok, windows.TokenOwner, nil, 0, &n)
	if n == 0 {
		t.Fatal("GetTokenInformation(TokenOwner) returned no size")
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(tok, windows.TokenOwner, &buf[0], n, &n); err != nil {
		t.Fatalf("GetTokenInformation(TokenOwner): %v", err)
	}
	owner := (*struct{ Owner *windows.SID })(unsafe.Pointer(&buf[0])).Owner
	return owner.String()
}

// ACE is one access control entry as read back from a DACL.
type ACE struct {
	Type  uint8
	Flags uint8
	Mask  uint32
	SID   string
}

// SD is a security descriptor as read back from a file.
type SD struct {
	Owner     string
	Protected bool
	NullDACL  bool
	ACEs      []ACE
}

// ReadSD reads the owner and DACL of path.
func ReadSD(t testing.TB, path string) SD {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", path, err)
	}
	return decodeSD(t, sd)
}

func decodeSD(t testing.TB, sd *windows.SECURITY_DESCRIPTOR) SD {
	t.Helper()
	var out SD
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatalf("Owner: %v", err)
	}
	out.Owner = owner.String()
	ctl, _, err := sd.Control()
	if err != nil {
		t.Fatalf("Control: %v", err)
	}
	out.Protected = ctl&windows.SE_DACL_PROTECTED != 0
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL: %v", err)
	}
	if dacl == nil {
		out.NullDACL = true
		return out
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatalf("GetAce: %v", err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		out.ACEs = append(out.ACEs, ACE{Type: ace.Header.AceType, Flags: ace.Header.AceFlags, Mask: uint32(ace.Mask), SID: sid.String()})
	}
	return out
}

// HasAllowACE reports whether sd has an allow ACE for sid.
func (sd SD) HasAllowACE(sid string) bool {
	for _, a := range sd.ACEs {
		if a.Type == windows.ACCESS_ALLOWED_ACE_TYPE && a.SID == sid {
			return true
		}
	}
	return false
}

// SetDACL replaces the DACL of path with the DACL of the SDDL string sddl
// (for example "D:P(A;;FA;;;S-1-5-18)"). A "P" in the SDDL makes it
// protected; otherwise inheritable ACEs of the parent are merged in.
func SetDACL(t testing.TB, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("SecurityDescriptorFromString(%q): %v", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL of %q: %v", sddl, err)
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if strings.Contains(sddl, "D:P") {
		info = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		t.Fatalf("SetNamedSecurityInfo(%s, %q): %v", path, sddl, err)
	}
}

// SetNullDACL gives path a NULL DACL (everyone has full access).
func SetNullDACL(t testing.TB, path string) {
	t.Helper()
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil); err != nil {
		t.Fatalf("SetNamedSecurityInfo(%s, NULL DACL): %v", path, err)
	}
}

// Icacls runs icacls with args and fails the test on error.
func Icacls(t testing.TB, args ...string) {
	t.Helper()
	out, err := exec.Command("icacls", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %v: %v: %s", args, err, out)
	}
}

// SetOwner makes sid the owner of path. Setting an owner other than
// oneself needs SeRestorePrivilege, which an elevated administrator token
// holds; it is enabled only on an impersonation token of this thread, so
// the rest of the test process is unaffected.
func SetOwner(t testing.TB, path, sid string) {
	t.Helper()
	s, err := windows.StringToSid(sid)
	if err != nil {
		t.Fatalf("StringToSid(%s): %v", sid, err)
	}
	err = withPrivilege("SeRestorePrivilege", func() error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, s, nil, nil, nil)
	})
	if err != nil {
		t.Fatalf("set owner of %s to %s (needs an elevated runner with SeRestorePrivilege): %v", path, sid, err)
	}
}

func withPrivilege(name string, f func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
		return err
	}
	defer func() { _ = windows.RevertToSelf() }()
	thread, err := windows.GetCurrentThread()
	if err != nil {
		return err
	}
	var tok windows.Token
	if err := windows.OpenThreadToken(thread, windows.TOKEN_QUERY|windows.TOKEN_ADJUST_PRIVILEGES, false, &tok); err != nil {
		return err
	}
	defer func() { _ = tok.Close() }()
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var tp windows.Tokenprivileges
	if err := windows.LookupPrivilegeValue(nil, p, &tp.Privileges[0].Luid); err != nil {
		return err
	}
	tp.PrivilegeCount = 1
	tp.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
	if err := windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil); err != nil {
		return err
	}
	// AdjustTokenPrivileges reports success with ERROR_NOT_ALL_ASSIGNED
	// in GetLastError when the token lacks the privilege; the operation
	// below then fails with access denied, which the caller reports.
	return f()
}

// ShortPath returns the 8.3 short form of path (GetShortPathName).
func ShortPath(t testing.TB, path string) string {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatalf("GetShortPathName(%s): %v", path, err)
	}
	if int(n) > len(buf) {
		buf = make([]uint16, n)
		if n, err = windows.GetShortPathName(p, &buf[0], uint32(len(buf))); err != nil {
			t.Fatalf("GetShortPathName(%s): %v", path, err)
		}
	}
	return windows.UTF16ToString(buf[:n])
}

// WithDefaultOwner runs f on this goroutine's OS thread under an
// impersonation copy of the process token whose default owner (TokenOwner)
// is sid. That reproduces what an elevated administrator token looks like
// under the default "Administrators group" owner policy, whatever the
// runner's own policy is. sid must be a group in the token that may own
// objects (Administrators, for an administrator). f must not start
// goroutines that create files: they would not run under this token.
func WithDefaultOwner(t testing.TB, sid string, f func()) {
	t.Helper()
	s, err := windows.StringToSid(sid)
	if err != nil {
		t.Fatalf("StringToSid(%s): %v", sid, err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
		t.Fatalf("ImpersonateSelf: %v", err)
	}
	defer func() { _ = windows.RevertToSelf() }()
	thread, err := windows.GetCurrentThread()
	if err != nil {
		t.Fatal(err)
	}
	var tok windows.Token
	if err := windows.OpenThreadToken(thread, windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT, false, &tok); err != nil {
		t.Fatalf("OpenThreadToken: %v", err)
	}
	defer func() { _ = tok.Close() }()
	owner := struct{ Owner *windows.SID }{Owner: s}
	if err := windows.SetTokenInformation(tok, windows.TokenOwner, (*byte)(unsafe.Pointer(&owner)), uint32(unsafe.Sizeof(owner))); err != nil {
		t.Fatalf("set the token's default owner to %s (the Windows job runs as an administrator): %v", sid, err)
	}
	f()
}
