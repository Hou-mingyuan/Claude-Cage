let messages = {};

export function setMessages(catalog) {
  messages = catalog;
}

export function formatMessage(message, ...values) {
  return values.length ? message.replace(/\{(\d+)\}/g, (match, index) =>
    Number(index) < values.length ? String(values[index]) : match,
  ) : message;
}

export function translate(source, ...values) {
  return formatMessage(Object.hasOwn(messages, source) ? messages[source] : source, ...values);
}
