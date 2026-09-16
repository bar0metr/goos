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
	available := -1
	for index := range table.bindings {
		entry := &table.bindings[index]
		if entry.used && entry.object == object && entry.root == root {
			return false
		}
		if !entry.used && available < 0 {
			available = index
		}
	}
	if available < 0 {
		return false
	}
	table.bindings[available] = endpointBinding{object: object, root: root, peer: peer, side: side, used: true}
	return true
}

// BindProcess installs one process endpoint and links it to an already bound
// opposite endpoint when present. Endpoint is encoded as one or two on the
// launch wire so zero remains "not an endpoint grant".
func (table *EndpointTable) BindProcess(object uint64, root uint64, endpoint uint8) bool {
	if endpoint < 1 || endpoint > 2 {
		return false
	}
	side := endpoint - 1
	if !table.Bind(object, root, 0, side) {
		return false
	}
	for index := range table.bindings {
		peer := &table.bindings[index]
		if peer.used && peer.object == object && peer.root != root && peer.side == side^1 {
			for ownIndex := range table.bindings {
				own := &table.bindings[ownIndex]
				if own.used && own.object == object && own.root == root {
					own.peer = peer.root
					peer.peer = root
					return true
				}
			}
		}
	}
	return true
}

func (table *EndpointTable) Unbind(object uint64, root uint64) {
	for index := range table.bindings {
		entry := &table.bindings[index]
		if entry.used && entry.object == object && entry.root == root {
			peerRoot := entry.peer
			*entry = endpointBinding{}
			if peerRoot != 0 {
				for peerIndex := range table.bindings {
					peer := &table.bindings[peerIndex]
					if peer.used && peer.object == object && peer.root == peerRoot {
						peer.peer = 0
					}
				}
			}
			return
		}
	}
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
