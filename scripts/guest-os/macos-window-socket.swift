import Foundation
import Darwin

// Socket waits use a dispatch worker, leaving Swift's cooperative executor
// available for window lookup, capture callbacks and connection cleanup.
final class MacWindowSocket: @unchecked Sendable {
    private let file: FileHandle
    private let io = DispatchQueue(label: "window.socket")

    init(_ fd: Int32, timeout: Double = 6) {
        file = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
        var limit = timeval(tv_sec: Int(timeout), tv_usec: Int32((timeout - floor(timeout)) * 1_000_000))
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &limit, socklen_t(MemoryLayout.size(ofValue: limit)))
        setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &limit, socklen_t(MemoryLayout.size(ofValue: limit)))
    }

    func close() { try? file.close() }

    func read(maximum: Int) async throws -> Data {
        try await withCheckedThrowingContinuation { continuation in
            io.async { [self] in
                var chunk = [UInt8](repeating: 0, count: maximum)
                var count: Int
                repeat { count = Darwin.read(file.fileDescriptor, &chunk, chunk.count) }
                while count < 0 && errno == EINTR
                if count < 0 {
                    continuation.resume(throwing: POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO))
                } else { continuation.resume(returning: Data(chunk.prefix(count))) }
            }
        }
    }

    func write(_ data: Data) async throws {
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            io.async { [self] in
                do { try file.write(contentsOf: data); continuation.resume() }
                catch { continuation.resume(throwing: error) }
            }
        }
    }
}
