package config

import "slices"

// legacyAgentToolName is the historical permission entry name of the
// sub-agent delegation tool, renamed to call_agent. It is recognized
// only at this migration boundary: loads read existing `agent`
// entries as call_agent, and new writes emit only call_agent. It is
// never a second live tool implementation.
const legacyAgentToolName = "agent"

// migrateLegacyPermissionName rewrites one permission tool-name list,
// replacing the legacy `agent` entry with DelegationToolName. If the
// list already names call_agent explicitly, that entry wins and the
// legacy one is dropped, so the migrated list carries the delegation
// tool exactly once; repeated legacy entries collapse the same way.
// Every other entry keeps its position.
func migrateLegacyPermissionName(list []string) []string {
	if !slices.Contains(list, legacyAgentToolName) {
		return list
	}
	out := make([]string, 0, len(list))
	claimed := slices.Contains(list, DelegationToolName)
	for _, name := range list {
		if name == legacyAgentToolName {
			if claimed {
				continue
			}
			out = append(out, DelegationToolName)
			claimed = true
			continue
		}
		out = append(out, name)
	}
	return out
}

// NormalizePermissionToolNames migrates legacy permission entries at
// the config load boundary: the permission allow list
// (permissions.allowed_tools) and the built-in deny list
// (options.disabled_tools) read `agent` as call_agent. It runs after
// every config merge, so migrated names are what the permission
// service and agent setup consume and what any later serialization
// writes back.
func (c *Config) NormalizePermissionToolNames() {
	if c.Permissions != nil {
		c.Permissions.AllowedTools = migrateLegacyPermissionName(c.Permissions.AllowedTools)
	}
	if c.Options != nil {
		c.Options.DisabledTools = migrateLegacyPermissionName(c.Options.DisabledTools)
	}
}
