//go:build windows

package workspaceconfig

// This is original Win32 ACL plumbing for the public CLI, not backend code.
// All security changes use handles; a reopened WRITE_DAC/WRITE_OWNER handle is
// checked against the anchored original before anything is changed. Win32
// errors are deliberately not returned: they can contain identifying details.
import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

const (
	securityOwner       = 0x00000001
	securityDACL        = 0x00000004
	securityProtectDACL = 0x80000000
	seFileObject        = 1
	seDACLPresent       = 0x0004
	seDACLProtected     = 0x1000
	readControl         = 0x00020000
	writeDACL           = 0x00040000
	writeOwner          = 0x00080000
	fileAllAccess       = 0x001f01ff
	fileReadAttributes  = 0x00000080
	aceInherited        = 0x10
	aclRevision         = 2
)

var (
	privateAdvapi       = syscall.NewLazyDLL("advapi32.dll")
	privateKernel       = syscall.NewLazyDLL("kernel32.dll")
	getSecurityInfoProc = privateAdvapi.NewProc("GetSecurityInfo")
	setSecurityInfoProc = privateAdvapi.NewProc("SetSecurityInfo")
	convertSDProc       = privateAdvapi.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	getSDDACLProc       = privateAdvapi.NewProc("GetSecurityDescriptorDacl")
	getSDControlProc    = privateAdvapi.NewProc("GetSecurityDescriptorControl")
	isValidACLProc      = privateAdvapi.NewProc("IsValidAcl")
	isValidSIDProc      = privateAdvapi.NewProc("IsValidSid")
	equalSIDProc        = privateAdvapi.NewProc("EqualSid")
	getACEProc          = privateAdvapi.NewProc("GetAce")
	finalPathProc       = privateKernel.NewProc("GetFinalPathNameByHandleW")
	volumeInfoProc      = privateKernel.NewProc("GetVolumeInformationByHandleW")
)

var errPrivateACL = errors.New("workspace Windows security requires an enforceable private owner-only ACL")

type tokenIdentity struct {
	user         *syscall.SID
	defaultOwner *syscall.SID
}

func currentTokenIdentity() (tokenIdentity, error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return tokenIdentity{}, errPrivateACL
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return tokenIdentity{}, errPrivateACL
	}
	sid, err := user.User.Sid.Copy()
	runtime.KeepAlive(user)
	if err != nil {
		return tokenIdentity{}, errPrivateACL
	}
	// An elevated token can default new objects' owner to Administrators.
	// That exact token-selected default is accepted only while securing a
	// newly created/owner-selected object, then replaced with the user SID.
	var size uint32
	err = syscall.GetTokenInformation(token, syscall.TokenOwner, nil, 0, &size)
	if err != syscall.ERROR_INSUFFICIENT_BUFFER || size < uint32(unsafe.Sizeof(uintptr(0))) || size > 64<<10 {
		return tokenIdentity{}, errPrivateACL
	}
	buffer := make([]byte, size)
	if syscall.GetTokenInformation(token, syscall.TokenOwner, &buffer[0], size, &size) != nil {
		return tokenIdentity{}, errPrivateACL
	}
	owner := *(**syscall.SID)(unsafe.Pointer(&buffer[0]))
	if !validSID(owner) {
		return tokenIdentity{}, errPrivateACL
	}
	defaultOwner, err := owner.Copy()
	runtime.KeepAlive(buffer)
	if err != nil {
		return tokenIdentity{}, errPrivateACL
	}
	return tokenIdentity{sid, defaultOwner}, nil
}

func validSID(sid *syscall.SID) bool {
	if sid == nil {
		return false
	}
	ok, _, _ := isValidSIDProc.Call(uintptr(unsafe.Pointer(sid)))
	runtime.KeepAlive(sid)
	return ok != 0
}

func sameSID(a, b *syscall.SID) bool {
	if !validSID(a) || !validSID(b) {
		return false
	}
	ok, _, _ := equalSIDProc.Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b)))
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
	return ok != 0
}

type fileSecurity struct {
	owner      *syscall.SID
	dacl       unsafe.Pointer
	descriptor unsafe.Pointer
}

func readFileSecurity(handle syscall.Handle) (fileSecurity, error) {
	var s fileSecurity
	status, _, _ := getSecurityInfoProc.Call(
		uintptr(handle), seFileObject, securityOwner|securityDACL,
		uintptr(unsafe.Pointer(&s.owner)), 0, uintptr(unsafe.Pointer(&s.dacl)), 0,
		uintptr(unsafe.Pointer(&s.descriptor)),
	)
	if status != 0 || s.descriptor == nil {
		s.close()
		return fileSecurity{}, errPrivateACL
	}
	return s, nil
}

func (s fileSecurity) close() {
	if s.descriptor != nil {
		_, _ = syscall.LocalFree(syscall.Handle(uintptr(s.descriptor)))
	}
}

func windowsFileInfo(handle syscall.Handle, directory bool) (syscall.ByHandleFileInformation, error) {
	var info syscall.ByHandleFileInformation
	kind, err := syscall.GetFileType(handle)
	if err != nil || kind != syscall.FILE_TYPE_DISK || syscall.GetFileInformationByHandle(handle, &info) != nil {
		return info, errPrivateACL
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		(info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return info, errPrivateACL
	}
	var volumeFlags uint32
	ok, _, _ := volumeInfoProc.Call(uintptr(handle), 0, 0, 0, 0, uintptr(unsafe.Pointer(&volumeFlags)), 0, 0)
	if ok == 0 || volumeFlags&0x00000008 /* FILE_PERSISTENT_ACLS */ == 0 {
		return info, errPrivateACL
	}
	return info, nil
}

// Write access isn't part of os.Root.Open's GENERIC_READ access mask.
// Reopen the final native path with security rights, not the caller's path,
// reject reparse points and check inode identity before modifying its ACL.
func writableSecurityHandle(f *os.File, directory, changeOwner bool) (syscall.Handle, error) {
	original := syscall.Handle(f.Fd())
	before, err := windowsFileInfo(original, directory)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	path := make([]uint16, 32768)
	n, _, _ := finalPathProc.Call(uintptr(original), uintptr(unsafe.Pointer(&path[0])), uintptr(len(path)), 0)
	if n == 0 || n >= uintptr(len(path)) {
		return syscall.InvalidHandle, errPrivateACL
	}
	access := uint32(readControl | writeDACL | fileReadAttributes)
	if changeOwner {
		access |= writeOwner
	}
	handle, err := syscall.CreateFile(
		&path[0], access,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
	runtime.KeepAlive(path)
	runtime.KeepAlive(f)
	if err != nil {
		return syscall.InvalidHandle, errPrivateACL
	}
	after, err := windowsFileInfo(handle, directory)
	if err != nil || before.VolumeSerialNumber != after.VolumeSerialNumber ||
		before.FileIndexHigh != after.FileIndexHigh || before.FileIndexLow != after.FileIndexLow {
		syscall.CloseHandle(handle)
		return syscall.InvalidHandle, errPrivateACL
	}
	return handle, nil
}

func ensurePrivatePermissions(f *os.File, directory bool) error {
	identity, err := currentTokenIdentity()
	if err != nil {
		return err
	}
	// An object that is already private is left as it is. Rewriting a
	// directory's DACL makes Windows propagate inheritance to its existing
	// children, which can race with a concurrent writer securing its new file
	// in that directory and strip that file's protection.
	if checkWindowsACL(syscall.Handle(f.Fd()), directory, identity, false) == nil {
		return nil
	}
	existing, err := readFileSecurity(syscall.Handle(f.Fd()))
	if err != nil {
		return err
	}
	changeOwner := !sameSID(existing.owner, identity.user)
	owned := !changeOwner || sameSID(existing.owner, identity.defaultOwner)
	existing.close()
	if !owned {
		return errPrivateACL
	}
	handle, err := writableSecurityHandle(f, directory, changeOwner)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(handle)
	// Re-check ownership on the identity-checked writable handle too.
	existing, err = readFileSecurity(handle)
	if err != nil {
		return err
	}
	owned = sameSID(existing.owner, identity.user) || (changeOwner && sameSID(existing.owner, identity.defaultOwner))
	existing.close()
	if !owned {
		return errPrivateACL
	}
	if err := setOwnerOnlyACL(handle, directory, identity.user, changeOwner); err != nil {
		return err
	}
	// Detect unsupported volumes and failed/corrupted ACL enforcement by
	// reading back the actual descriptor, not by trusting SetSecurityInfo.
	return checkWindowsACL(handle, directory, identity, false)
}

func setOwnerOnlyACL(handle syscall.Handle, directory bool, owner *syscall.SID, changeOwner bool) error {
	sid, err := owner.String()
	if err != nil {
		return errPrivateACL
	}
	flags := ""
	if directory {
		flags = "OICI" // future child directories and files inherit owner access
	}
	sddl, err := syscall.UTF16PtrFromString("O:" + sid + "D:P(A;" + flags + ";FA;;;" + sid + ")")
	if err != nil {
		return errPrivateACL
	}
	var descriptor unsafe.Pointer
	ok, _, _ := convertSDProc.Call(uintptr(unsafe.Pointer(sddl)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
	runtime.KeepAlive(sddl)
	if ok == 0 || descriptor == nil {
		return errPrivateACL
	}
	defer syscall.LocalFree(syscall.Handle(uintptr(descriptor)))
	var present, defaulted int32
	var dacl unsafe.Pointer
	ok, _, _ = getSDDACLProc.Call(uintptr(descriptor), uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)), uintptr(unsafe.Pointer(&defaulted)))
	if ok == 0 || present == 0 || dacl == nil {
		return errPrivateACL
	}
	information := uintptr(securityDACL | securityProtectDACL)
	var ownerPointer uintptr
	if changeOwner {
		information |= securityOwner
		ownerPointer = uintptr(unsafe.Pointer(owner))
	}
	status, _, _ := setSecurityInfoProc.Call(uintptr(handle), seFileObject,
		information, ownerPointer, 0, uintptr(dacl), 0)
	runtime.KeepAlive(owner)
	if status != 0 {
		return errPrivateACL
	}
	return nil
}

type aclHeader struct {
	Revision  uint8
	Reserved  uint8
	Size      uint16
	ACECount  uint16
	Reserved2 uint16
}

type allowACE struct {
	Type  uint8
	Flags uint8
	Size  uint16
	Mask  uint32
	// SID starts immediately after Mask, at offset eight.
}

func checkWindowsACL(handle syscall.Handle, directory bool, identity tokenIdentity, allowInherited bool) error {
	if _, err := windowsFileInfo(handle, directory); err != nil {
		return err
	}
	s, err := readFileSecurity(handle)
	if err != nil {
		return err
	}
	defer s.close()
	owned := sameSID(s.owner, identity.user)
	if allowInherited {
		owned = owned || sameSID(s.owner, identity.defaultOwner)
	}
	if !owned || s.dacl == nil {
		return errPrivateACL
	}
	var control uint16
	var revision uint32
	ok, _, _ := getSDControlProc.Call(uintptr(s.descriptor), uintptr(unsafe.Pointer(&control)), uintptr(unsafe.Pointer(&revision)))
	if ok == 0 || control&seDACLPresent == 0 || (!allowInherited && control&seDACLProtected == 0) {
		return errPrivateACL
	}
	ok, _, _ = isValidACLProc.Call(uintptr(s.dacl))
	if ok == 0 {
		return errPrivateACL
	}
	header := (*aclHeader)(s.dacl)
	if header.Revision != aclRevision || header.ACECount != 1 {
		return errPrivateACL
	}
	var ace unsafe.Pointer
	ok, _, _ = getACEProc.Call(uintptr(s.dacl), 0, uintptr(unsafe.Pointer(&ace)))
	if ok == 0 || ace == nil {
		return errPrivateACL
	}
	offset := uintptr(ace) - uintptr(s.dacl)
	if offset < unsafe.Sizeof(aclHeader{}) || offset+unsafe.Sizeof(allowACE{}) > uintptr(header.Size) {
		return errPrivateACL
	}
	entry := (*allowACE)(ace)
	if entry.Type != 0 || entry.Size < 8+8 || offset+uintptr(entry.Size) > uintptr(header.Size) || entry.Mask != fileAllAccess {
		return errPrivateACL
	}
	wantFlags := uint8(0)
	if directory {
		wantFlags = 3
	}
	flags := entry.Flags
	if allowInherited {
		flags &^= aceInherited
	}
	if flags != wantFlags {
		return errPrivateACL
	}
	aceSID := (*syscall.SID)(unsafe.Add(ace, 8))
	if !validSID(aceSID) || 8+aceSID.Len() != int(entry.Size) || !sameSID(aceSID, identity.user) {
		return errPrivateACL
	}
	return nil
}

func checkPrivatePermissions(f *os.File, directory bool) error {
	identity, err := currentTokenIdentity()
	if err != nil {
		return err
	}
	err = checkWindowsACL(syscall.Handle(f.Fd()), directory, identity, false)
	runtime.KeepAlive(f)
	return err
}

func prepareExistingPrivateLock(f *os.File) error {
	identity, err := currentTokenIdentity()
	if err != nil {
		return err
	}
	if err := checkWindowsACL(syscall.Handle(f.Fd()), false, identity, false); err == nil {
		return nil
	}
	// A creator's empty lock inherits only the private parent's owner ACE.
	// Finalize that ACL safely; never bless an Everyone/group/null DACL.
	if err := checkWindowsACL(syscall.Handle(f.Fd()), false, identity, true); err != nil {
		return err
	}
	return ensurePrivatePermissions(f, false)
}

func privateFilePathSafe(path string) bool {
	// Alternate streams share a file's identity/ACL but are not standalone
	// regular files. Reject stream syntax instead of securing the base file.
	return !strings.Contains(strings.TrimPrefix(path, filepath.VolumeName(path)), ":")
}
