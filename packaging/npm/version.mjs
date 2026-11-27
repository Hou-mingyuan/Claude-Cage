const numeric = /^(?:0|[1-9][0-9]*)$/;
const syntax = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/;

export function parseVersion(version) {
  if (typeof version !== 'string' || version !== version.trim()) throw new Error('Expected a canonical SemVer without build metadata');
  const match = syntax.exec(version);
  if (!match) throw new Error(`Invalid version: ${version}`);
  const prerelease = match[4] ? match[4].split('.') : [];
  if (prerelease.some(part => /^[0-9]+$/.test(part) && !numeric.test(part))) throw new Error(`Invalid numeric prerelease identifier: ${version}`);
  return { major: BigInt(match[1]), minor: BigInt(match[2]), patch: BigInt(match[3]), prerelease };
}

function compare(a, b) {
  return a === b ? 0 : a < b ? -1 : 1;
}

export function compareVersions(a, b) {
  const left = parseVersion(a);
  const right = parseVersion(b);
  for (const part of ['major', 'minor', 'patch']) {
    const order = compare(left[part], right[part]);
    if (order) return order;
  }
  if (!left.prerelease.length || !right.prerelease.length) {
    return compare(Number(!left.prerelease.length), Number(!right.prerelease.length));
  }
  for (let index = 0; index < Math.max(left.prerelease.length, right.prerelease.length); index++) {
    const x = left.prerelease[index];
    const y = right.prerelease[index];
    if (x === undefined || y === undefined) return compare(left.prerelease.length, right.prerelease.length);
    const xNumeric = numeric.test(x);
    const yNumeric = numeric.test(y);
    const order = xNumeric && yNumeric ? compare(BigInt(x), BigInt(y))
      : xNumeric !== yNumeric ? (xNumeric ? -1 : 1) : compare(x, y);
    if (order) return order;
  }
  return 0;
}
