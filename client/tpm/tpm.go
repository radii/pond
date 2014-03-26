// Package tpm wraps the Trousers library for accessing the TPM from
// user-space. It currently provides very limited functionality: just NVRAM
// access.
package tpm

func Present() bool {
	return false
}

func isError(result int) bool {
	return true
}

type Error struct {
	result int
}

func (e Error) Error() string {
	return "tpm disabled"
}

type ErrorCode int

func (e Error) Code() ErrorCode {
	return ErrorCode(0)
}

// Error code values
const (
	// Failed to connect to daemon process.
	ErrCodeCommunicationFailure ErrorCode = 0
	// The TPM is disabled in the BIOS.
	ErrCodeTPMDisabled = 7
	// The TPM doesn't have an owner and thus no storage root key has been
	// defined.
	ErrCodeNoStorageRootKey = 0x12
	// The NVRAM index already exists.
	ErrCodeNVRAMAlreadyExists = 0x13b
	// The password is incorrect.
	ErrCodeAuthentication = 1
)

type Object struct {
	result int 
}

type Policy struct {
	policy int
}

func (p *Policy) SetKey(key [20]byte) error {
	return nil
}

func (p *Policy) SetPassword(pw string) error {
	return nil
}

func (p *Policy) AssignTo(o *Object) error {
	return nil
}

type NVRAM struct {
	Object
	Index       uint32
	Size        int
	Permissions uint32
}

const (
	PermAuthRead       = 1
	PermAuthWrite      = 2
	PermWriteAllAtOnce = 3
)

func (nv *NVRAM) setAttributes() error {
	return nil
}

func (nv *NVRAM) Create() error {
	return nil
}

func (nv *NVRAM) Destroy() error {
	return nil
}

func (nv *NVRAM) Read(out []byte) (int, error) {
	return 0, nil
}

func (nv *NVRAM) Write(contents []byte) error {
	return nil
}

type RSA struct {
	Object
}

func (rsa *RSA) GetPolicy() (*Policy, error) {
	return nil, nil
}

type Context struct {
	foo int
}

func NewContext() (*Context, error) {
	return nil, nil
}

func (c *Context) Close() error {
	return nil
}

func (c *Context) GetPolicy() (*Policy, error) {
	return nil, nil
}

func (c *Context) NewPolicy() (*Policy, error) {
	return nil, nil
}

func (c *Context) NewNVRAM() (*NVRAM, error) {
	return nil, nil
}

func (c *Context) NewRSA() (*RSA, error) {
	return nil, nil
}

func (c *Context) TakeOwnership(srk *RSA) error {
	return nil
}
