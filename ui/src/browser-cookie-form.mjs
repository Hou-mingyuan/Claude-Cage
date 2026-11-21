// Form state for adding or editing one cookie of the tab's own site. The
// guest validates again; these helpers only decide what the form offers.

export const siteHostname = url => {
  try {
    const parsed = new URL(url);
    return parsed.protocol === "http:" || parsed.protocol === "https:" ? parsed.hostname : "";
  } catch { return ""; }
};

// A bare domain is host-only; a leading dot also covers subdomains.
export const cookieForHost = (cookie, hostname) => Boolean(hostname) && (cookie.domain.startsWith(".")
  ? hostname === cookie.domain.slice(1) || hostname.endsWith(cookie.domain)
  : hostname === cookie.domain);

export const cookieEditable = (cookie, hostname) =>
  !cookie.partitionKey && !cookie.partitionKeyOpaque && cookieForHost(cookie, hostname);

const pad = number => String(number).padStart(2, "0");
// Formats seconds since the epoch for a datetime-local input in local time.
export const localDateTime = seconds => {
  const date = new Date(seconds * 1000);
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
};

export const newCookieForm = (url, now = Date.now()) => ({
  name: "", value: "", domain: siteHostname(url), path: "/", secure: url.startsWith("https:"), httpOnly: false,
  sameSite: "Lax", session: true, expires: localDateTime(now / 1000 + 30 * 86400),
});

export const cookieForm = (cookie, value, now = Date.now()) => ({
  name: cookie.name, value, domain: cookie.domain, path: cookie.path, secure: Boolean(cookie.secure),
  httpOnly: Boolean(cookie.httpOnly), sameSite: cookie.sameSite || "", session: Boolean(cookie.session),
  expires: localDateTime(cookie.session ? now / 1000 + 30 * 86400 : cookie.expires),
});

// Returns the cookie sent to the guest; expires 0 means a session cookie.
export const cookieFromForm = form => {
  const expires = form.session ? 0 : Math.floor(new Date(form.expires).getTime() / 1000);
  return {
    name: form.name, value: form.value, domain: form.domain.trim().toLowerCase(), path: form.path,
    secure: form.secure, httpOnly: form.httpOnly, ...(form.sameSite ? { sameSite: form.sameSite } : {}),
    expires: Number.isFinite(expires) ? expires : -1,
  };
};
