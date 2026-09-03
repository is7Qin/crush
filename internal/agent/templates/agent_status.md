Report the lifecycle status of an agent task by task id.

<usage>
- Provide the task id returned by a call_agent delegation
- Shows status, profile, model, child session id, and the summary or error once the task terminalizes
- Only tasks owned by the current session are visible; unknown or foreign ids return an error
</usage>
