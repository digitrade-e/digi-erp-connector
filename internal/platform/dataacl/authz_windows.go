//go:build windows

package dataacl

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// AuthZ (authz.dll): what an account effectively may do with an entry, from
// the entry's descriptor and the account's groups — the carry-over's
// "before" (carry.go). Not in x/sys.

var (
	modAuthz                           = windows.NewLazySystemDLL("authz.dll")
	procAuthzInitializeResourceManager = modAuthz.NewProc("AuthzInitializeResourceManager")
	procAuthzInitializeContextFromSid  = modAuthz.NewProc("AuthzInitializeContextFromSid")
	procAuthzAccessCheck               = modAuthz.NewProc("AuthzAccessCheck")
	procAuthzGetInformationFromContext = modAuthz.NewProc("AuthzGetInformationFromContext")
	procAuthzFreeContext               = modAuthz.NewProc("AuthzFreeContext")
	procAuthzFreeResourceManager       = modAuthz.NewProc("AuthzFreeResourceManager")
)

const (
	authzRMFlagNoAudit         = 0x1
	authzContextInfoGroupsSids = 2 // AUTHZ_CONTEXT_INFORMATION_CLASS AuthzContextInfoGroupsSids
	maximumAllowed             = 0x02000000
	seGroupEnabled             = 0x4
	seGroupUseForDenyOnly      = 0x10
)

// authzAccessRequest is AUTHZ_ACCESS_REQUEST.
type authzAccessRequest struct {
	DesiredAccess        uint32
	PrincipalSelfSid     *windows.SID
	ObjectTypeList       uintptr
	ObjectTypeListLength uint32
	OptionalArguments    uintptr
}

// authzAccessReply is AUTHZ_ACCESS_REPLY for one result.
type authzAccessReply struct {
	ResultListLength      uint32
	GrantedAccessMask     *uint32
	SaclEvaluationResults *uint32
	Error                 *uint32
}

// authzRM is an AuthZ resource manager (no audit).
type authzRM struct{ h uintptr }

func newAuthzRM() (*authzRM, error) {
	if err := procAuthzInitializeResourceManager.Find(); err != nil {
		return nil, err
	}
	var h uintptr
	r, _, err := procAuthzInitializeResourceManager.Call(authzRMFlagNoAudit, 0, 0, 0, 0, uintptr(unsafe.Pointer(&h)))
	if r == 0 {
		return nil, fmt.Errorf("dataacl: AuthzInitializeResourceManager: %w", err)
	}
	return &authzRM{h: h}, nil
}

func (rm *authzRM) close() {
	if rm != nil && rm.h != 0 {
		procAuthzFreeResourceManager.Call(rm.h)
		rm.h = 0
	}
}

// authzCtx is one account's AuthZ context, and its SIDs for allows (user
// and enabled groups) and for denies (every group, deny-only ones too).
type authzCtx struct {
	h           uintptr
	sid         string
	allow, deny map[string]bool
}

// context resolves sid's groups (a domain account's from its domain
// controller: this is what fails when none answers).
func (rm *authzRM) context(sid string) (*authzCtx, error) {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return nil, err
	}
	var h uintptr
	// LUID Identifier is passed by value: one 8-byte slot on amd64 and arm64,
	// two 4-byte ones on 386.
	args := []uintptr{0, uintptr(unsafe.Pointer(s)), rm.h, 0, 0}
	if unsafe.Sizeof(uintptr(0)) == 4 {
		args = append(args, 0)
	}
	args = append(args, 0, uintptr(unsafe.Pointer(&h)))
	r, _, callErr := procAuthzInitializeContextFromSid.Call(args...)
	runtime.KeepAlive(s)
	if r == 0 {
		return nil, fmt.Errorf("resolve the groups of %s: %w", sid, callErr)
	}
	c := &authzCtx{h: h, sid: sid, allow: map[string]bool{sid: true}, deny: map[string]bool{sid: true}}
	if err := c.readGroups(); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *authzCtx) readGroups() error {
	var need uint32
	procAuthzGetInformationFromContext.Call(c.h, authzContextInfoGroupsSids, 0, uintptr(unsafe.Pointer(&need)), 0)
	if need == 0 {
		return errors.New("dataacl: AuthZ answered no group list")
	}
	buf := make([]byte, need)
	r, _, err := procAuthzGetInformationFromContext.Call(c.h, authzContextInfoGroupsSids, uintptr(need), uintptr(unsafe.Pointer(&need)), uintptr(unsafe.Pointer(&buf[0])))
	if r == 0 {
		return fmt.Errorf("dataacl: read the groups of %s: %w", c.sid, err)
	}
	groups := (*windows.Tokengroups)(unsafe.Pointer(&buf[0]))
	for _, g := range groups.AllGroups() {
		s := g.Sid.String()
		c.deny[s] = true
		if g.Attributes&seGroupEnabled != 0 && g.Attributes&seGroupUseForDenyOnly == 0 {
			c.allow[s] = true
		}
	}
	runtime.KeepAlive(buf)
	return nil
}

func (c *authzCtx) close() {
	if c != nil && c.h != 0 {
		procAuthzFreeContext.Call(c.h)
		c.h = 0
	}
}

// access is what the account may do with an object whose descriptor holds
// owner, group and the raw dacl (its generic rights mapped first).
func (c *authzCtx) access(owner, group *windows.SID, dacl []byte) (uint32, error) {
	mapped, err := MapGenericACL(dacl)
	if err != nil {
		return 0, err
	}
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return 0, err
	}
	if err := sd.SetOwner(owner, false); err != nil {
		return 0, err
	}
	if group != nil {
		if err := sd.SetGroup(group, false); err != nil {
			return 0, err
		}
	}
	if err := sd.SetDACL((*windows.ACL)(unsafe.Pointer(&mapped[0])), true, false); err != nil {
		return 0, err
	}
	req := authzAccessRequest{DesiredAccess: maximumAllowed}
	var granted, status uint32
	reply := authzAccessReply{ResultListLength: 1, GrantedAccessMask: &granted, Error: &status}
	r, _, callErr := procAuthzAccessCheck.Call(0, c.h, uintptr(unsafe.Pointer(&req)), 0, uintptr(unsafe.Pointer(sd)), 0, 0, uintptr(unsafe.Pointer(&reply)), 0)
	runtime.KeepAlive(mapped)
	runtime.KeepAlive(sd)
	runtime.KeepAlive(owner)
	runtime.KeepAlive(group)
	if r == 0 {
		return 0, fmt.Errorf("dataacl: AuthzAccessCheck for %s: %w", c.sid, callErr)
	}
	if status != 0 {
		return 0, nil // ERROR_ACCESS_DENIED: nothing at all
	}
	return granted, nil
}
