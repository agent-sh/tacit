package record

// The local layer's view of the store's records (phase 2, answer 7): a
// machine that never writes the store (the Mac) reads the store's records
// as they come in, and may hold its own record of a store folder in its
// overlay. That local record overrides the folder's status, and the history
// that led to it, for this machine only: its kind, name, aliases,
// suppression, tools, commands and repos stay the store's. A local record
// of a folder the store has no record of is this machine's own folder.

// MergeLocal is the records this machine reads: the store's, each with the
// local layer's status where the layer holds a record of the same folder,
// kind and name, and the layer's own folders. Neither map is changed.
func MergeLocal(store, local map[string]*Record) map[string]*Record {
	out := make(map[string]*Record, len(store)+len(local))
	for f, r := range store {
		out[f] = r
	}
	for f, l := range local {
		s := store[f]
		switch {
		case s == nil:
			out[f] = l
		case s.Kind == l.Kind && s.Name == l.Name && l.Status != "" && (l.Status != s.Status || l.Successor != s.Successor):
			out[f] = Override(s, l)
		}
	}
	return out
}

// Override is store's record with local's status, history and successor
// (the status's): what this
// machine reads of the folder. LocalStatus marks it.
func Override(store, local *Record) *Record {
	m := store.Clone()
	m.Status = local.Status
	m.Was = append([]Entry(nil), local.Was...)
	m.Successor = local.Successor
	m.LocalStatus = true
	return m
}

// AsOverride is the record a local layer writes to override store's status:
// the store's record with the new status, history and this machine's
// evidence, every other field the store's.
func AsOverride(store, decided *Record) *Record {
	m := store.Clone()
	m.Status = decided.Status
	m.Was = append([]Entry(nil), decided.Was...)
	m.Successor = decided.Successor
	m.Evidence = append([]Evidence(nil), decided.Evidence...)
	m.LocalStatus = false
	return m
}
