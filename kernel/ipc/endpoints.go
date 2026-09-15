package ipc

const MaxEndpoints = MaxChannels * 2

type endpointBinding struct {
	object uint64
	root   uint64
	peer   uint64
	side   uint8
	used   bool
}

type EndpointTable struct {
	bindings [MaxEndpoints]endpointBinding
}

func (table *EndpointTable) Bind(object uint64, root uint64, peer uint64, side uint8) bool {
	if object == 0 || root == 0 || side > 1 {
		return false
	}
	for index := range table.bindings {
		entry := &table.bindings[index]
		if entry.used && entry.object == object && entry.root == root {
			return false
		}
		if !entry.used {
			*entry = endpointBinding{object: object, root: root, peer: peer, side: side, used: true}
			return true
		}
	}
	return false
}

func (table *EndpointTable) Lookup(object uint64, root uint64) (uint8, uint64, bool) {
	if object == 0 || root == 0 {
		return 0, 0, false
	}
	for index := range table.bindings {
		entry := &table.bindings[index]
		if entry.used && entry.object == object && entry.root == root {
			return entry.side, entry.peer, true
		}
	}
	return 0, 0, false
}
