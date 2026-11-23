export function eligibleAutoAccounts(accounts) {
  return Array.isArray(accounts)
    ? accounts.filter((account) => account.enabled && account.status === "ready")
    : [];
}

export function autoSelectionUnavailable(accounts) {
  return Array.isArray(accounts) && eligibleAutoAccounts(accounts).length === 0;
}

export function defaultTerminalAccount(accounts) {
  if (!Array.isArray(accounts)) return "auto";
  if (eligibleAutoAccounts(accounts).length > 0) return "auto";
  return accounts.find((account) => account.status === "ready")?.id || "login";
}

export function reconcileTerminalAccount({ account, accounts, manual }) {
  if (!Array.isArray(accounts)) return account;
  if (!manual) return defaultTerminalAccount(accounts);
  if (account === "login") return "login";
  if (account === "auto") {
    return eligibleAutoAccounts(accounts).length > 0
      ? "auto"
      : defaultTerminalAccount(accounts);
  }
  return accounts.some((item) => item.id === account)
    ? account
    : defaultTerminalAccount(accounts);
}
