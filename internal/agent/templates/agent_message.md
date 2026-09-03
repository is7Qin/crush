Send a follow-up message to a call_agent child conversation by task id.

<usage>
- Provide the task id returned by one of your own call_agent delegations
- The message is committed to the child's FIFO mailbox and the tool returns as soon as the row is accepted; it never waits for the child to run
- Appending to a terminal task schedules a new attempt on the retained child session and reports its task id when created immediately
- This queues a message; it does not answer a pending child question
- Only tasks owned by the current session are addressable; unknown or foreign ids return an error
</usage>
