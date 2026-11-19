/*
 * Cage workbench pixel wheel.
 * Licensed under MPL 2.0 (see LICENSE.txt)
 */

// Cage's own display server takes the scrolled distance in one message,
// where RFB sends a press and a release for each fixed step.
export const cageWheelEncoding = -57345;
export const cageWheelCapability = 'cage-wheel-v1';
export const cageWheelMessage = 180;
export const cageWheelLimit = 2048;

// type, padding, x, y, then the signed horizontal and vertical distance.
export function cageWheelPacket(x, y, dx, dy) {
    const packet = new Uint8Array(14), view = new DataView(packet.buffer);
    packet[0] = cageWheelMessage;
    view.setUint16(2, x);
    view.setUint16(4, y);
    view.setInt32(6, dx);
    view.setInt32(10, dy);
    return packet;
}

// A notched wheel reports whole steps: its legacy delta is a multiple of 120.
// A trackpad and a free-spinning wheel report the distance they moved.
export function cageWheelNotched(event) {
    if (event.deltaMode !== 0) return true;
    const steps = event.wheelDeltaY || event.wheelDeltaX || 0;
    return Math.abs(steps) >= 120 && steps % 120 === 0;
}

// A trackpad reports many small distances, and each message is a round trip
// to the guest. The first goes out at once; later ones are added up and sent
// once per frame. Distance beyond the limit of one message stays for the next.
// A send that returns a promise reports back: a few of those may be under way
// at once, so a distant guest still scrolls every frame, and beyond that the
// distance is added up until one of them has settled.
export const cageWheelPending = 3;
export default class CageWheel {
    constructor(send, schedule = callback => requestAnimationFrame(callback),
                cancel = handle => cancelAnimationFrame(handle)) {
        this._send = send;
        this._schedule = schedule;
        this._cancel = cancel;
        this._x = this._y = this._dx = this._dy = 0;
        this._frame = null;
        this._pending = 0;
    }

    add(x, y, dx, dy) {
        this._x = x;
        this._y = y;
        this._dx += dx;
        this._dy += dy;
        if (this._frame === null && this._pending < cageWheelPending) this._flush();
    }

    // The distance not sent yet, which the caller now delivers another way.
    take() {
        const rest = { dx: this._dx, dy: this._dy };
        this._dx = this._dy = 0;
        return rest;
    }

    _flush() {
        const dx = Math.max(-cageWheelLimit, Math.min(cageWheelLimit, Math.round(this._dx)));
        const dy = Math.max(-cageWheelLimit, Math.min(cageWheelLimit, Math.round(this._dy)));
        // Less than a pixel waits for the next event.
        if (!dx && !dy) return;
        this._dx -= dx;
        this._dy -= dy;
        const sent = this._send(this._x, this._y, dx, dy);
        if (sent && typeof sent.then === 'function') {
            const settled = () => {
                this._pending--;
                if (this._frame === null) this._flush();
            };
            this._pending++;
            sent.then(settled, settled);
        }
        this._frame = this._schedule(() => {
            this._frame = null;
            if (this._pending < cageWheelPending) this._flush();
        });
    }

    stop() {
        if (this._frame !== null) this._cancel(this._frame);
        this._frame = null;
        this._dx = this._dy = 0;
    }
}
