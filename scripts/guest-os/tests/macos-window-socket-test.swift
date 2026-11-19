import Foundation
import Darwin

@main
struct WindowSocketTest {
    static func pair() -> [Int32] {
        var descriptors = [Int32](repeating: -1, count: 2)
        precondition(socketpair(AF_UNIX, SOCK_STREAM, 0, &descriptors) == 0)
        return descriptors
    }

    static func main() async throws {
        // Run with LIBDISPATCH_COOPERATIVE_POOL_STRICT=1 as well. An idle
        // socket must not block the executor that services other connections.
        let pairs = (0..<16).map { _ in pair() }
        let readers = pairs.map { descriptors in Task {
            let connection = MacWindowSocket(descriptors[0], timeout: 1)
            defer { connection.close() }
            do { _ = try await connection.read(maximum: 1); preconditionFailure("idle peer did not time out") }
            catch { precondition((error as? POSIXError)?.code == .EAGAIN) }
        } }
        let started = ProcessInfo.processInfo.systemUptime
        try await Task.sleep(nanoseconds: 50_000_000)
        let heartbeat = ProcessInfo.processInfo.systemUptime - started
        precondition(heartbeat < 0.5, "idle sockets blocked Swift's cooperative executor")
        for reader in readers { await reader.value }
        for descriptors in pairs {
            precondition(fcntl(descriptors[0], F_GETFD) == -1 && errno == EBADF)
            Darwin.close(descriptors[1])
        }
        for _ in 0..<48 {
            let descriptors = pair()
            let connection = MacWindowSocket(descriptors[0])
            let peer = FileHandle(fileDescriptor: descriptors[1], closeOnDealloc: true)
            try peer.write(contentsOf: Data("request\n".utf8))
            let request = try await connection.read(maximum: 64)
            precondition(request == Data("request\n".utf8))
            try await connection.write(Data("reply\n".utf8))
            let reply = try peer.read(upToCount: 6)
            precondition(reply == Data("reply\n".utf8))
            try peer.close()
            let ended = try await connection.read(maximum: 64)
            precondition(ended.isEmpty)
            connection.close()
            precondition(fcntl(descriptors[0], F_GETFD) == -1 && errno == EBADF)
        }
        print("16 idle sockets: 50ms heartbeat resumed in \(Int(heartbeat * 1000))ms; timeouts, 48 exchanges and disconnect cleanup passed")
    }
}
