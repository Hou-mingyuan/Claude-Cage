package workbench

// accountEpochKey carries the unlock epoch admitted by the HTTP handler.
// Operations must not gain authority from a later unlock while waiting.
type accountEpochKey struct{}
