package storage

// InvalidateMetadataCache drops directory listings without making a WPS call.
// Account changes and explicit search refreshes must not reuse an earlier
// credential's metadata just because a directory cache TTL has not expired.
func (s *Storage) InvalidateMetadataCache() { s.invalidate() }

func (m *MultiSpace) InvalidateMetadataCache() {
	m.mu.Lock()
	spaces := make([]*Storage, 0, len(m.spaces)+1)
	if m.single != nil {
		spaces = append(spaces, m.single)
	}
	for _, space := range m.spaces {
		spaces = append(spaces, space)
	}
	m.mu.Unlock()
	for _, space := range spaces {
		space.InvalidateMetadataCache()
	}
}
