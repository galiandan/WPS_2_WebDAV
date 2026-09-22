package httpserver

// MemberStorage is the full storage facade of a user-visible subtree. All
// mutating and read methods enforce the user's current policy independently
// of the HTTP route guard.
type MemberStorage interface {
	RESTReadStorage
	RESTMutations
	UploadStorage
}

// NewMemberDispatcher constructs independent mutable request services without
// copying mutexes/Once fields. Expensive render and editor permits remain shared
// with the administrator; search metadata and task visibility stay scoped.
func (d *RESTDispatcher) NewMemberDispatcher(storage MemberStorage, downloads DownloadStorage, identity func() ([32]byte, error), owner string, version uint64, name string) (*RESTDispatcher, error) {
	names := &RootNameController{current: name}
	member, err := NewRESTDispatcher(d.limits, names, d.session, storage, d.status, d.download, downloads, storage, storage, d.locks, d.maxUploadBytes)
	if err != nil {
		return nil, err
	}
	member.webAuth = d.webAuth
	member.searchInvalidator = d.searchInvalidator
	member.taskOwnerID, member.taskPolicyVersion = owner, version
	member.taskIdentity = identity
	member.tasks = d.tasks
	member.taskOwnerResolver = d.taskOwnerResolver
	member.SetSearchIdentity(identity)
	index := member.searchIndex()
	index.limits.MaxBytes = 4 << 20
	index.limits.MaxEntries = 5000
	index.limits.MaxFolders = 1000
	index.globalGate = d.searchIndex().globalGate
	member.thumbnailOnce.Do(func() { member.thumbnails = d.thumbnailState() })
	member.textOnce.Do(func() { member.textEditor = d.editorState() })
	return member, nil
}

// NewMemberDAV uses the same lock namespace and transfer budgets as the
// administrator, but accepts only the already scoped storage facade.
func (d *RESTDispatcher) NewMemberDAV(storage DAVStorage, downloads DownloadStorage, uploads UploadStorage, mutations DAVMutations, propfind DAVLimits, prefix string) (*DAVDispatcher, error) {
	return NewDAVDispatcher(storage, d.limits, propfind, d.download, downloads, uploads, mutations, d.locks, d.maxUploadBytes, prefix)
}

// StopMemberServices stops scanning when a cached user service is evicted.
func (d *RESTDispatcher) StopMemberServices() { d.CancelSearch() }
