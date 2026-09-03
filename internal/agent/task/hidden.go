package task

// HiddenProfile is the fixed internal profile the agentic_fetch tool
// admits its child runs under. It is not user-configurable and never
// resolvable through the public profile selection. A task admitted
// under this profile is hidden: excluded from every public task
// list/status/output/cancel/message/question control path, readable
// only through Manager.DiagnosticTask with the trusted originating
// workspace and owner session.
//
// config duplicates this name for profile-validation reservation
// because internal/config must not import the task package.
const HiddenProfile = "agentic_fetch_internal"

// IsHidden reports whether the record is a system-owned hidden task.
// Hiding is keyed on the fixed admission profile, which is immutable
// after admission, so no public control path can launder a hidden
// task into visibility.
func (t *Task) IsHidden() bool {
	return t != nil && t.Profile == HiddenProfile
}
