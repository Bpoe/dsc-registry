package cli

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func currentSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}

func createPrivate(path string) (*os.File, error) {
	sid, err := currentSID()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;;FA;;;" + sid.String() + ")")
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	attrs := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, attrs, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func checkPrivate(f *os.File) error {
	current, err := currentSID()
	if err != nil {
		return invalid("cannot determine current user for credential ACL validation")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return invalid("cannot inspect API key file ACL")
	}
	owner, _, err := sd.Owner()
	if err != nil || !owner.Equals(current) {
		return invalid("API key file must be owned by the current user")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return invalid("API key file must have a user-private ACL")
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return invalid("cannot inspect API key file ACE")
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return invalid("API key file has an unsupported ACL entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(current) && !sid.IsWellKnown(windows.WinLocalSystemSid) {
			return invalid("API key file grants access to another principal; use a user-private ACL")
		}
	}
	return nil
}
