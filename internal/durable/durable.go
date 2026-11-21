// Package durable makes a completed file replacement survive a power loss.
package durable

// SyncDir flushes dir's entries to disk, so a file just renamed or linked
// into it is still there under its new name after a power loss; fsyncing
// the file itself only keeps its content. Call it after the rename.
//
// File systems that cannot sync a directory are not an error. On Windows it
// does nothing: directories cannot be opened for flushing there, and NTFS
// journals the rename itself. It is a variable so tests elsewhere can see
// which directories a write flushes.
var SyncDir = syncDir
