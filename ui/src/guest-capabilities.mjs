// Local workbenches can install or repair components that are not ready yet.
// The headless Windows server only implements the features it advertises.
export function guestCapability(status, name) {
  return status?.guest_system !== "windows-remote" ||
    status.guest_capabilities?.available?.[name] === true;
}
