package capability

type Rights uint32

const (
	Read Rights = 1 << iota
	Write
	Execute
	Transfer
	Wait
)

type Capability struct {
	object uint64
	rights Rights
}

func New(object uint64, rights Rights) Capability {
	return Capability{object: object, rights: rights}
}

func (cap Capability) Object() uint64 { return cap.object }

func (cap Capability) Rights() Rights { return cap.rights }

func (cap Capability) Allows(required Rights) bool {
	return required != 0 && cap.rights&required == required
}

func (cap Capability) Derive(rights Rights) (Capability, bool) {
	if rights == 0 || cap.rights&rights != rights {
		return Capability{}, false
	}
	return New(cap.object, rights), true
}
