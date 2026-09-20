package chat

import (
	"github.com/charmbracelet/crush/internal/message"
)

// BodyLoader fetches the source messages an item needs to rebuild
// a released body: one message for assistant, user, and shell
// items, the assistant message plus its tool message for tool
// items. Loaders are built by the UI layer, which owns store
// access; items only retain the closure so this package stays free
// of workspace imports. A nil loader pins the item: it is never
// released. It reports false when a source message is gone.
type BodyLoader func() ([]message.Message, bool)

// Releasable is implemented by message items whose decoded body can
// be dropped when the item sits far from the viewport and reloaded
// on demand when it scrolls back. Releasing only discards big
// strings (decoded parts, rendered caches); small metadata (IDs,
// finished state) stays so list geometry and freeze decisions keep
// working without a reload.
type Releasable interface {
	MessageItem
	// SetBodyLoader attaches the reload closure. It does not load.
	SetBodyLoader(loader BodyLoader)
	// ReleaseBody drops the decoded body when the item is finished
	// and reloadable. Live (unfinished) items and items without a
	// loader are left alone. It never bumps the version, so valid
	// list-level memos keep serving identical bytes.
	ReleaseBody()
	// EnsureBody reloads a released body. Safe to call on every
	// render; a loaded item is a no-op.
	EnsureBody()
	// BodyLoaded reports whether the decoded body is resident.
	BodyLoaded() bool
}

// bodyUnavailableText is rendered when a released body can no longer
// be reloaded (its source message is gone). Deletion paths remove
// the item itself, so this is unreachable in practice; it exists
// only to avoid a nil dereference.
const bodyUnavailableText = "(message unavailable)"
