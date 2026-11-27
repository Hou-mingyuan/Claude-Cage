import Cocoa
import CryptoKit
import Security
import Darwin

let cageUpdateBundleID = "io.github.lee-xeo.claude-cage"
// Pinned official Apple Developer ID Team for eligible unsigned source builds
// upgrading in-place to the official signed release. Signed installs always
// pin strictly to their own running Team ID.
let cageOfficialAppleTeamID = "WBR2J2NFHN"

enum CageProfile {
    static var clientOnly: Bool { Bundle.main.object(forInfoDictionaryKey: "CageClientOnly") as? Bool == true }
    static var intelMacTest: Bool { Bundle.main.bundleIdentifier == cageUpdateBundleID + ".intel-macos-test" }
    static func configURL() -> URL {
        if let path = ProcessInfo.processInfo.environment["CLAUDE_CAGE_CONFIG"], !path.isEmpty {
            return URL(fileURLWithPath: path).standardizedFileURL
        }
        if intelMacTest {
            return FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".claude-cage-intel-test/config.json")
        }
        return FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(clientOnly ? ".claude-cage/client/config.json" : ".claude-cage/config.json")
    }

    // Every helper and relaunched app inherits the same absolute profile, even
    // when its working directory differs from the original launch directory.
    static func pinEnvironment() { setenv("CLAUDE_CAGE_CONFIG", configURL().path, 1) }

    static func relaunchEnvironment() -> [String: String] {
        var selected = ["CLAUDE_CAGE_CONFIG": configURL().path]
        for key in ["HOME", "CFFIXED_USER_HOME"] {
            if let value = ProcessInfo.processInfo.environment[key], value.hasPrefix("/") { selected[key] = value }
        }
        return selected
    }
}

struct CageUpdateBusy: LocalizedError {
    var errorDescription: String? { "此安装或配置正由其他 Cage 工作台或更新器使用。请在已打开的工作台更新，或退出后重试 cage update。" }
}

/// All profiles using an installed bundle coordinate on the same kernel locks.
/// Locks on the bundle/Contents directories never modify a signed app.
final class CageUpdateLock {
    private let descriptor: Int32
    private init(fd: Int32, shared: Bool = false) throws {
        guard fd >= 0 else { throw CageUpdateError("无法访问当前 Cage 安装目录") }
        if flock(fd, (shared ? LOCK_SH : LOCK_EX) | LOCK_NB) != 0 {
            let busy = errno == EWOULDBLOCK
            close(fd)
            if busy { throw CageUpdateBusy() }
            throw CageUpdateError("无法锁定 Cage 更新任务，请检查安装位置后重试。")
        }
        descriptor = fd
    }
    convenience init(directory: URL, shared: Bool = false) throws {
        try self.init(fd: open(directory.path, O_RDONLY | O_DIRECTORY | O_CLOEXEC), shared: shared)
    }
    deinit { close(descriptor) }

    // A source build and a released app may point at the same profile. Each
    // must protect its shared download-resume and install results as well.
    static func profile(_ root: URL, installing: Bool) throws -> CageUpdateLock {
        let size = confstr(_CS_DARWIN_USER_TEMP_DIR, nil, 0)
        guard size > 0 else { throw CageUpdateError("无法访问当前用户的临时目录") }
        var path = [CChar](repeating: 0, count: size)
        guard confstr(_CS_DARWIN_USER_TEMP_DIR, &path, size) > 0 else { throw CageUpdateError("无法访问当前用户的临时目录") }
        let name = SHA256.hash(data: Data(root.resolvingSymlinksInPath().standardizedFileURL.path.utf8)).map { String(format: "%02x", $0) }.joined()
        let file = URL(fileURLWithPath: String(cString: path)).appendingPathComponent("cage-update-" + name + (installing ? ".install" : ".download"))
        return try CageUpdateLock(fd: open(file.path, O_RDONLY | O_CREAT | O_CLOEXEC | O_NOFOLLOW, 0o600))
    }
    static func worker(for bundle: URL, downloadsAt root: URL? = nil) throws -> [CageUpdateLock] {
        let worker = try CageUpdateLock(directory: bundle.appendingPathComponent("Contents"))
        try checkInstaller(for: bundle)
        guard let root else { return [worker] }
        let download = try profile(root, installing: false)
        _ = try profile(root, installing: true)
        return [worker, download]
    }

    // The installer claims this directory in BOTH the old and new bundle before
    // replying ready. New callers therefore see the guard across replacement.
    static func installer(for bundle: URL) throws -> CageUpdateLock {
        try CageUpdateLock(directory: bundle.appendingPathComponent("Contents/MacOS"))
    }
    static func checkInstaller(for bundle: URL) throws { _ = try installer(for: bundle) }
}

enum CageUpdateProcess {
    static func executable(_ pid: pid_t) -> URL? {
        var path = [CChar](repeating: 0, count: 4096)
        guard proc_pidpath(pid, &path, UInt32(path.count)) > 0 else { return nil }
        return URL(fileURLWithPath: String(cString: path)).resolvingSymlinksInPath()
    }
    static func isNativeParent(_ pid: pid_t, target: URL) -> Bool {
        let expected = target.appendingPathComponent("Contents/MacOS/Cage")
        guard pid == getppid(), expected.path == expected.resolvingSymlinksInPath().path,
              executable(pid)?.path == expected.path else { return false }
        if let registered = NSRunningApplication(processIdentifier: pid)?.bundleURL {
            return registered.resolvingSymlinksInPath().path == target.path
        }
        return true
    }
    static func hasOtherWindow(_ bundle: URL) -> Bool {
        let executablePath = bundle.resolvingSymlinksInPath().appendingPathComponent("Contents/MacOS/Cage").path
        return NSWorkspace.shared.runningApplications.contains {
            $0.processIdentifier != getpid() && $0.bundleURL?.resolvingSymlinksInPath().path == bundle.resolvingSymlinksInPath().path &&
                executable($0.processIdentifier)?.path == executablePath
        }
    }
}

final class CageUpdateValidation {
    private let lock = NSLock()
    private let completion = DispatchGroup()
    private var cancelled = false
    private var process: Process?

    init() { completion.enter() }
    func checkCancellation() throws {
        lock.lock(); defer { lock.unlock() }
        if cancelled { throw CageUpdateError("更新验证已取消") }
    }
    var isCancelled: Bool { lock.lock(); defer { lock.unlock() }; return cancelled }
    func start(_ value: Process) throws {
        lock.lock(); defer { lock.unlock() }
        if cancelled { throw CageUpdateError("更新验证已取消") }
        try value.run()
        process = value
    }
    func released(_ value: Process) {
        lock.lock(); defer { lock.unlock() }
        if process === value { process = nil }
    }
    func cancel() {
        lock.lock(); cancelled = true; let active = process; lock.unlock()
        if let active { CageUpdateFiles.stop(active) }
    }
    func finish() { completion.leave() }
    func wait() { completion.wait() }
}

struct CageUpdateError: LocalizedError {
    let message: String
    var errorDescription: String? { message }
    init(_ message: String) { self.message = message }
}

struct CageUpdatePlan: Codable {
    let version: String
    let team: String
    let target: String
    let candidate: String
    let backup: String
    let parentPID: Int32
    let resultPath: String
    let unsignedSource: Bool
    let requiresCommitReceipt: Bool

    init(version: String, team: String, target: String, candidate: String, backup: String, parentPID: Int32, resultPath: String, unsignedSource: Bool = false, requiresCommitReceipt: Bool = false) {
        self.version = version
        self.team = team
        self.target = target
        self.candidate = candidate
        self.backup = backup
        self.parentPID = parentPID
        self.resultPath = resultPath
        self.unsignedSource = unsignedSource
        self.requiresCommitReceipt = requiresCommitReceipt
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        version = try container.decode(String.self, forKey: .version)
        team = try container.decode(String.self, forKey: .team)
        target = try container.decode(String.self, forKey: .target)
        candidate = try container.decode(String.self, forKey: .candidate)
        backup = try container.decode(String.self, forKey: .backup)
        parentPID = try container.decode(Int32.self, forKey: .parentPID)
        resultPath = try container.decode(String.self, forKey: .resultPath)
        unsignedSource = try container.decodeIfPresent(Bool.self, forKey: .unsignedSource) ?? false
        requiresCommitReceipt = try container.decodeIfPresent(Bool.self, forKey: .requiresCommitReceipt) ?? false
    }
}

enum CageUpdateVersion {
    static func isNewer(_ candidate: String, than current: String) -> Bool {
        func parts(_ text: String) -> ([Int], [String])? {
            guard text.range(of: "^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\\.[0-9A-Za-z-]+)*)?$", options: .regularExpression) != nil else { return nil }
            let sections = text.split(separator: "-", maxSplits: 1).map(String.init)
            let numbers = sections[0].split(separator: ".").compactMap { Int($0) }
            guard numbers.count == 3 else { return nil }
            let pre = sections.count == 2 ? sections[1].split(separator: ".").map(String.init) : []
            if pre.contains(where: { $0.count > 1 && $0.first == "0" && $0.allSatisfy(\.isNumber) }) { return nil }
            return (numbers, pre)
        }
        guard let (a, ap) = parts(candidate), let (b, bp) = parts(current) else { return false }
        for i in 0..<3 where a[i] != b[i] { return a[i] > b[i] }
        if ap.isEmpty || bp.isEmpty { return ap.isEmpty && !bp.isEmpty }
        for i in 0..<min(ap.count, bp.count) where ap[i] != bp[i] {
            let x = ap[i], y = bp[i], xn = x.allSatisfy(\.isNumber), yn = y.allSatisfy(\.isNumber)
            if xn && yn { return x.count == y.count ? x > y : x.count > y.count }
            if xn != yn { return !xn }
            return x > y
        }
        return ap.count > bp.count
    }
}

enum CageUpdateFiles {
    static func cachedArchive(in root: URL) -> URL {
        root.resolvingSymlinksInPath().appendingPathComponent("download-resume/Cage.dmg")
    }
    static func discardCachedArchive(in root: URL) {
        let archive = cachedArchive(in: root)
        guard archive.resolvingSymlinksInPath().path == archive.path else { return }
        try? FileManager.default.removeItem(at: archive)
        _ = rmdir(archive.deletingLastPathComponent().path)
    }
    static func installerReply(from input: FileHandle, within seconds: TimeInterval? = nil) -> [String: Any]? {
        let deadline = seconds.map { ProcessInfo.processInfo.systemUptime + $0 }
        var data = Data()
        while data.count < 16384 {
            if let deadline {
                let remaining = deadline - ProcessInfo.processInfo.systemUptime
                guard remaining > 0 else { return nil }
                var descriptor = pollfd(fd: input.fileDescriptor, events: Int16(POLLIN), revents: 0)
                let result = poll(&descriptor, 1, Int32(ceil(remaining * 1000)))
                if result == -1 && errno == EINTR { continue }
                guard result > 0 else { return nil }
            }
            guard let byte = try? input.read(upToCount: 1), let value = byte.first else { return nil }
            if value == 10 { return (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] }
            data.append(value)
        }
        return nil
    }
    static func parentHasExited(_ parentPID: pid_t) -> Bool {
        getppid() != parentPID
    }
    static func waitForParentExit(_ parentPID: pid_t, timeout: TimeInterval) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while !parentHasExited(parentPID) && Date() < deadline {
            Thread.sleep(forTimeInterval: 0.05)
        }
        return parentHasExited(parentPID)
    }
    static func restore(backup: URL, target: URL, manager: FileManager, beforeReplace: (URL) throws -> Void = { _ in }) throws {
        let copy = backup.deletingLastPathComponent().appendingPathComponent("restore-" + UUID().uuidString + ".app")
        try FileManager.default.copyItem(at: backup, to: copy)
        defer { try? FileManager.default.removeItem(at: copy) }
        try beforeReplace(copy)
        if FileManager.default.fileExists(atPath: target.path) {
            _ = try manager.replaceItemAt(target, withItemAt: copy, backupItemName: nil, options: [.usingNewMetadataOnly])
        } else {
            try manager.moveItem(at: copy, to: target)
        }
    }
    static func replaceAndVerify(target: URL, candidate: URL, backup: URL, manager: FileManager, onReplace: () -> Void = {}, restoreOriginal: (() throws -> Void)? = nil, verify: (URL) throws -> Void) throws {
        do {
            _ = try manager.replaceItemAt(target, withItemAt: candidate, backupItemName: nil, options: [.usingNewMetadataOnly])
            onReplace()
            try verify(target)
        } catch {
            let detail = error.localizedDescription
            do {
                if let restoreOriginal { try restoreOriginal() }
                else { try restore(backup: backup, target: target, manager: manager) }
            }
            catch { throw CageUpdateError("安装与自动恢复均未完成，原应用备份已保留：\(detail)") }
            throw CageUpdateError("安装验证失败，已恢复原应用：\(detail)")
        }
    }
    static func privateDirectory(_ url: URL) throws {
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let values = try url.resourceValues(forKeys: [.isSymbolicLinkKey, .isDirectoryKey])
        guard values.isDirectory == true, values.isSymbolicLink != true else { throw CageUpdateError("更新目录无效") }
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: url.path)
    }
    static func write(_ data: Data, to url: URL) throws {
        try data.write(to: url, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }
    static func digest(_ url: URL, validation: CageUpdateValidation? = nil) throws -> String {
        let file = try FileHandle(forReadingFrom: url)
        defer { try? file.close() }
        var hash = SHA256()
        while let data = try file.read(upToCount: 1024 * 1024), !data.isEmpty { try validation?.checkCancellation(); hash.update(data: data) }
        return hash.finalize().map { String(format: "%02x", $0) }.joined()
    }
    static func stop(_ process: Process) {
        guard process.isRunning else { return }
        process.terminate()
        DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + 2) {
            if process.isRunning { Darwin.kill(process.processIdentifier, SIGKILL) }
        }
    }
    static func run(_ executable: String, _ arguments: [String], timeout: TimeInterval = 90, validation: CageUpdateValidation? = nil) throws -> Data {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: executable)
        process.arguments = arguments
        let pipe = Pipe(); process.standardOutput = pipe; process.standardError = pipe
        let expiration = DispatchWorkItem { stop(process) }
        if let validation { try validation.start(process) } else { try process.run() }
        defer { validation?.released(process) }
        DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + timeout, execute: expiration)
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit(); expiration.cancel()
        try validation?.checkCancellation()
        guard process.terminationStatus == 0, data.count <= 1024 * 1024 else { throw CageUpdateError("本机更新验证未完成，请重试") }
        return data
    }
    static func signingInfo(_ url: URL, bundle: Bool, expectedTeam: String? = nil, currentApp: Bool = false) throws -> String {
        var code: SecStaticCode?
        guard SecStaticCodeCreateWithPath(url as CFURL, [], &code) == errSecSuccess, let code else {
            throw CageUpdateError(currentApp ? "无法读取当前应用的 Apple 签名" : "无法读取更新应用的 Apple 签名")
        }
        var requirement: SecRequirement?
        let identity = bundle ? " and identifier \"\(cageUpdateBundleID)\"" : ""
        let team = expectedTeam.map { " and certificate leaf[subject.OU] = \"\($0)\"" } ?? ""
        let expression = "anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists\(identity)\(team)"
        guard SecRequirementCreateWithString(expression as CFString, [], &requirement) == errSecSuccess else { throw CageUpdateError("无法建立发布者签名要求") }
        let flags = SecCSFlags(rawValue: kSecCSStrictValidate | kSecCSCheckAllArchitectures | kSecCSCheckNestedCode)
        let status = SecStaticCodeCheckValidity(code, flags, requirement)
        guard status == errSecSuccess else {
            if currentApp {
                throw CageUpdateError("当前 macOS 应用没有可验证的 Apple 开发者签名（状态码 \(status)），无法自动安装更新")
            }
            throw CageUpdateError("更新应用的 Apple 签名无效或发布者不匹配（\(url.lastPathComponent)，状态码 \(status)）")
        }
        var information: CFDictionary?
        guard SecCodeCopySigningInformation(code, SecCSFlags(rawValue: kSecCSSigningInformation), &information) == errSecSuccess,
              let info = information as? [String: Any], let actual = info[kSecCodeInfoTeamIdentifier as String] as? String,
              actual.range(of: "^[A-Z0-9]{10}$", options: .regularExpression) != nil else { throw CageUpdateError("当前开发构建没有可验证的发布者，无法自动安装更新") }
        if let expectedTeam, actual != expectedTeam { throw CageUpdateError("更新应用的发布者不匹配") }
        return actual
    }
    static func isInstalledApplicationsPath(_ url: URL) -> Bool {
        let resolved = url.resolvingSymlinksInPath().standardizedFileURL
        guard resolved.pathExtension == "app" else { return false }
        let parent = resolved.deletingLastPathComponent().resolvingSymlinksInPath().standardizedFileURL.path
        let systemApps = URL(fileURLWithPath: "/Applications", isDirectory: true).resolvingSymlinksInPath().standardizedFileURL.path
        let userApps = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Applications", isDirectory: true).resolvingSymlinksInPath().standardizedFileURL.path
        return parent == systemApps || parent == userApps
    }
    static func verifyUnsignedAppIdentity(_ url: URL, version: String? = nil) throws {
        let values = try url.resourceValues(forKeys: [.isDirectoryKey, .isSymbolicLinkKey])
        guard values.isDirectory == true, values.isSymbolicLink != true, url.pathExtension == "app" else {
            throw CageUpdateError("当前应用路径无效，无法自动更新")
        }
        let plist = url.appendingPathComponent("Contents/Info.plist")
        guard let info = try PropertyListSerialization.propertyList(from: Data(contentsOf: plist), format: nil) as? [String: Any],
              info["CFBundleIdentifier"] as? String == cageUpdateBundleID,
              info["CageClientOnly"] as? Bool != true,
              info["CFBundleExecutable"] as? String == "Cage" else {
            throw CageUpdateError("当前应用身份不匹配，无法自动切换为官方签名版本")
        }
        if let version, info["CageVersion"] as? String != version {
            throw CageUpdateError("原应用版本不匹配")
        }
        let control = url.appendingPathComponent("Contents/Resources/cage")
        guard FileManager.default.isExecutableFile(atPath: control.path) else {
            throw CageUpdateError("当前应用缺少必要组件，无法自动更新")
        }
    }
    static func verifyInstalledOriginal(_ url: URL, version: String, team: String, unsignedSource: Bool) throws {
        if unsignedSource {
            try verifyUnsignedAppIdentity(url, version: version)
        } else {
            try verifyApp(url, version: version, team: team, gatekeeper: false)
        }
    }
    static func verifyArchitecture(_ executable: URL, validation: CageUpdateValidation? = nil) throws {
        #if arch(x86_64)
        let architecture = "x86_64"
        #else
        let architecture = "arm64"
        #endif
        _ = try run("/usr/bin/lipo", [executable.path, "-verify_arch", architecture], validation: validation)
    }
    static func verifyApp(_ url: URL, version: String, team: String, gatekeeper: Bool = true, validation: CageUpdateValidation? = nil) throws {
        try validation?.checkCancellation()
        let values = try url.resourceValues(forKeys: [.isDirectoryKey, .isSymbolicLinkKey])
        guard values.isDirectory == true, values.isSymbolicLink != true, url.pathExtension == "app" else { throw CageUpdateError("更新包中缺少有效的 Cage.app") }
        let plist = url.appendingPathComponent("Contents/Info.plist")
        guard let info = try PropertyListSerialization.propertyList(from: Data(contentsOf: plist), format: nil) as? [String: Any],
              info["CFBundleIdentifier"] as? String == cageUpdateBundleID,
              info["CageVersion"] as? String == version,
              info["CFBundleExecutable"] as? String == "Cage" else { throw CageUpdateError("更新包的应用身份或版本不匹配") }
        if let minimum = info["LSMinimumSystemVersion"] as? String {
            let parts = minimum.split(separator: ".").compactMap { Int($0) }
            guard minimum.range(of: "^[0-9]+(\\.[0-9]+){0,2}$", options: .regularExpression) != nil,
                  (1...3).contains(parts.count), ProcessInfo.processInfo.isOperatingSystemAtLeast(OperatingSystemVersion(majorVersion: parts[0], minorVersion: parts.count > 1 ? parts[1] : 0, patchVersion: parts.count > 2 ? parts[2] : 0)) else { throw CageUpdateError("此更新需要更高版本的 macOS") }
        }
        _ = try signingInfo(url, bundle: true, expectedTeam: team)
        try verifyArchitecture(url.appendingPathComponent("Contents/MacOS/Cage"), validation: validation)
        for name in ["cage", "mihomo", "CageUpdateInstaller"] {
            let nested = url.appendingPathComponent("Contents/Resources/\(name)")
            guard FileManager.default.fileExists(atPath: nested.path) else { throw CageUpdateError("更新包缺少必要的应用组件") }
            _ = try signingInfo(nested, bundle: false, expectedTeam: team)
        }
        if gatekeeper { _ = try run("/usr/sbin/spctl", ["--assess", "--type", "execute", "--verbose=2", url.path], validation: validation) }
        try validation?.checkCancellation()
    }
    static func validateTree(_ app: URL, validation: CageUpdateValidation? = nil) throws {
        let base = app.resolvingSymlinksInPath().path + "/"
        guard let items = FileManager.default.enumerator(at: app, includingPropertiesForKeys: [.isRegularFileKey, .fileSizeKey, .isSymbolicLinkKey], options: []) else { throw CageUpdateError("无法检查更新包内容") }
        var total: Int64 = 0
        var count = 0
        for case let entry as URL in items {
            try validation?.checkCancellation()
            count += 1
            guard count <= 50000, entry.resolvingSymlinksInPath().path.hasPrefix(base) else { throw CageUpdateError("更新包包含越界文件或过多文件") }
            let values = try entry.resourceValues(forKeys: [.isRegularFileKey, .fileSizeKey])
            if values.isRegularFile == true { total += Int64(values.fileSize ?? 0) }
            guard total <= 1024 * 1024 * 1024 else { throw CageUpdateError("解包后的应用超过 1 GiB") }
        }
    }
    static func withMountedImage<T>(archive: URL, directory: URL, validation: CageUpdateValidation? = nil, body: (URL) throws -> T) throws -> T {
        let mount = directory.appendingPathComponent("mount", isDirectory: true)
        try privateDirectory(mount)
        defer {
            if (try? run("/usr/bin/hdiutil", ["detach", mount.path], timeout: 30)) == nil {
                _ = try? run("/usr/bin/hdiutil", ["detach", "-force", mount.path], timeout: 30)
            }
        }
        _ = try run("/usr/bin/hdiutil", ["attach", "-readonly", "-nobrowse", "-noautoopen", "-mountpoint", mount.path, archive.path], validation: validation)
        try validation?.checkCancellation()
        return try body(mount)
    }
    static func extract(archive: URL, hash: String, version: String, team: String, directory: URL, validation: CageUpdateValidation? = nil) throws -> URL {
        guard try digest(archive, validation: validation) == hash else { throw CageUpdateError("安装包下载后发生变化，请重新下载") }
        return try withMountedImage(archive: archive, directory: directory, validation: validation) { mount in
            let source = mount.appendingPathComponent("Cage.app", isDirectory: true)
            try validateTree(source, validation: validation)
            try verifyApp(source, version: version, team: team, validation: validation)
            let target = directory.appendingPathComponent("Cage.app", isDirectory: true)
            _ = try run("/usr/bin/ditto", [source.path, target.path], validation: validation)
            try validateTree(target, validation: validation)
            try verifyApp(target, version: version, team: team, validation: validation)
            return target
        }
    }
}

/// The terminal can authorize an install only after it receives a verified
/// "ready" event. The native installer remains the owner of replacement and rollback.
final class CageCLIUpdate {
    private let bundle: URL
    private var windowLock: CageUpdateLock?
    private var finishing = false
    private var awaitingConsent = false
    private var signals: [DispatchSourceSignal] = []
    private lazy var updater = CageAppUpdater(bundleURL: bundle, onChange: { [weak self] state in self?.receive(state) },
                                              quit: {}, reconnect: {}, headless: true)
    private init(bundle: URL) { self.bundle = bundle.resolvingSymlinksInPath() }

    static func run() -> Never {
        CageProfile.pinEnvironment()
        signal(SIGPIPE, SIG_IGN)
        let session = CageCLIUpdate(bundle: Bundle.main.bundleURL)
        session.start()
        return withExtendedLifetime(session) { dispatchMain() }
    }

    private func start() {
        do {
            windowLock = try CageUpdateLock(directory: bundle)
            if CageUpdateProcess.hasOtherWindow(bundle) {
                receive(["phase": "waiting", "message": "此安装的 Cage 工作台已打开。请在工作台更新，或退出后重试 cage update。"])
                return
            }
        } catch {
            receive(["phase": error is CageUpdateBusy ? "waiting" : "failed", "message": error.localizedDescription])
            return
        }
        for number in [SIGINT, SIGTERM] {
            signal(number, SIG_IGN)
            let source = DispatchSource.makeSignalSource(signal: number, queue: .main)
            source.setEventHandler { [weak self] in self?.finish(code: 1) }
            source.resume()
            signals.append(source)
        }
        _ = updater.handle("install")
    }

    static func readConsent(from input: FileHandle) -> Bool {
        for expected in "INSTALL\n".utf8 {
            guard let byte = try? input.read(upToCount: 1), byte.first == expected else { return false }
        }
        return true
    }

    private func receive(_ state: [String: Any]) {
        guard !finishing, let phase = state["phase"] as? String else { return }
        let fields = ["phase", "message", "currentVersion", "latestVersion", "receivedBytes", "totalBytes", "officialUpgrade", "retryAt", "retryable"]
        do {
            var data = try JSONSerialization.data(withJSONObject: state.filter { fields.contains($0.key) })
            data.append(10)
            try FileHandle.standardOutput.write(contentsOf: data)
        } catch {
            finish(code: 1)
            return
        }
        switch phase {
        case "ready":
            guard !awaitingConsent else { return }
            awaitingConsent = true
            DispatchQueue.global(qos: .userInitiated).async {
                let authorized = Self.readConsent(from: .standardInput)
                DispatchQueue.main.async {
                    guard !self.finishing else { return }
                    if authorized && !CageUpdateProcess.hasOtherWindow(self.bundle) { _ = self.updater.handle("complete") }
                    else { self.receive(["phase": "waiting", "message": authorized ? "Cage 工作台已打开，请在工作台更新或退出后重试。" : "尚未确认安装，原应用已保留。"]) }
                }
            }
        case "current", "manual", "waiting", "handed_off": finish(code: 0)
        case "failed": finish(code: 1)
        default: break
        }
    }

    private func finish(code: Int32) {
        guard !finishing else { return }
        finishing = true
        if !updater.prepareForTermination({ exit(code) }) { exit(code) }
    }
}

/// Watches one process ID via kqueue EVFILT_PROC/NOTE_EXIT so background
/// helpers never rely on cached NSRunningApplication.isTerminated state.
final class CageUpdateProcessMonitor {
    private let pid: pid_t
    private var kq: Int32
    private var exited: Bool

    init(pid: pid_t) {
        self.pid = pid
        let fd = kqueue()
        self.kq = fd
        self.exited = false
        guard fd >= 0 else {
            self.exited = kill(pid, 0) != 0 && errno == ESRCH
            return
        }
        var change = kevent(
            ident: UInt(pid),
            filter: Int16(EVFILT_PROC),
            flags: UInt16(EV_ADD | EV_ENABLE | EV_ONESHOT),
            fflags: UInt32(NOTE_EXIT),
            data: 0,
            udata: nil
        )
        let rc = kevent(fd, &change, 1, nil, 0, nil)
        if rc < 0 && errno == ESRCH {
            self.exited = true
            close(fd)
            self.kq = -1
        }
    }

    deinit {
        if kq >= 0 { close(kq) }
    }

    var hasExited: Bool {
        if exited { return true }
        if kq < 0 {
            if kill(pid, 0) != 0 && errno == ESRCH { exited = true }
            return exited
        }
        var event = kevent()
        var zero = timespec(tv_sec: 0, tv_nsec: 0)
        let n = kevent(kq, nil, 0, &event, 1, &zero)
        if n > 0 || (kill(pid, 0) != 0 && errno == ESRCH) {
            exited = true
            close(kq)
            kq = -1
        }
        return exited
    }

    func waitForExit(timeout: TimeInterval) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while !hasExited && Date() < deadline {
            let remaining = deadline.timeIntervalSinceNow
            if remaining <= 0 { break }
            if kq >= 0 {
                let slice = min(remaining, 0.25)
                let secs = Int(slice)
                let nsecs = Int((slice - Double(secs)) * 1_000_000_000)
                var ts = timespec(tv_sec: secs, tv_nsec: max(0, nsecs))
                var event = kevent()
                let n = kevent(kq, nil, 0, &event, 1, &ts)
                if n > 0 {
                    exited = true
                    close(kq)
                    kq = -1
                    return true
                }
            } else {
                Thread.sleep(forTimeInterval: min(remaining, 0.05))
            }
        }
        return hasExited
    }
}

/// All transitions for a desktop or terminal update run on the main queue.
final class CageAppUpdater {
    private static let unsignedManualMessage = "当前 macOS 应用未签名或签名无法验证，请从官方发布页下载安装包，退出 Cage 后替换原应用；配置、凭据、项目和虚拟机磁盘会保留。"
    private let bundleURL: URL
    private let root: URL
    private let onChange: ([String: Any]) -> Void
    private let quit: () -> Void
    private let reconnect: () -> Void
    private let readTeam: () throws -> String
    private let canUpgradeUnsigned: () -> Bool
    private let headless: Bool
    private var ownership: [CageUpdateLock]?
    private var state: [String: Any]
    private var worker: Process?
    private var validationTask: CageUpdateValidation?
    private var installer: Process?
    private var installerInput: Pipe?
    private var prepared: (directory: URL, app: URL, version: String, team: String, unsignedSource: Bool)?
    private var generation = 0
    private var launchAcknowledged = false
    private var workbenchInitialized = false
    private var waitingForTermination = false
    private var installationCommitted = false

    init(bundleURL: URL, onChange: @escaping ([String: Any]) -> Void, quit: @escaping () -> Void, reconnect: @escaping () -> Void, readTeam: (() throws -> String)? = nil, canUpgradeUnsigned: (() -> Bool)? = nil, headless: Bool = false) {
        let resolved = bundleURL.resolvingSymlinksInPath()
        self.bundleURL = resolved
        self.headless = headless
        self.onChange = onChange; self.quit = quit; self.reconnect = reconnect
        self.readTeam = readTeam ?? { try CageUpdateFiles.signingInfo(resolved, bundle: true, currentApp: true) }
        self.canUpgradeUnsigned = canUpgradeUnsigned ?? {
            guard ProcessInfo.processInfo.environment["CAGE_DISABLE_OFFICIAL_UPGRADE"] != "1" else { return false }
            guard CageUpdateFiles.isInstalledApplicationsPath(resolved) else { return false }
            return (try? CageUpdateFiles.verifyUnsignedAppIdentity(resolved)) != nil
        }
        root = CageProfile.configURL().deletingLastPathComponent().appendingPathComponent("updates", isDirectory: true)
        let version = Bundle(url: bundleURL)?.object(forInfoDictionaryKey: "CageVersion") as? String ?? "开发版本"
        state = ["phase": "idle", "currentVersion": version, "message": ""]
        let resultURL = root.appendingPathComponent("last-result.json")
        if !headless, let data = try? Data(contentsOf: resultURL), data.count < 16384,
           let result = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
            state["message"] = result["message"] as? String ?? ""
            if result["success"] as? Bool == false { state["phase"] = "failed"; state["retryable"] = true }
            try? FileManager.default.removeItem(at: resultURL)
        }
    }
    func handle(_ operation: String) -> [String: Any] {
        if Bundle(url: bundleURL)?.bundleIdentifier == cageUpdateBundleID + ".intel-macos-test" {
            if state["phase"] as? String != "manual" {
                change(["phase": "manual", "message": "Intel 测试版通过独立测试包更新。"])
            }
            return state
        }
        if Bundle(url: bundleURL)?.object(forInfoDictionaryKey: "CageClientOnly") as? Bool == true {
            if state["phase"] as? String != "manual" || operation != "status" {
                change(["phase": "manual", "message": "轻量客户端使用独立安装包更新；连接配置会保留，云端任务不受影响。"])
            }
            return state
        }
        let busy = ["checking", "downloading", "downloaded", "verifying", "installing"].contains(state["phase"] as? String ?? "")
        if ["check", "install"].contains(operation), let value = state["retryAt"] as? String,
           let date = ISO8601DateFormatter().date(from: value), date > Date() { return state }
        switch operation {
        case "status": break
        case "check": if !busy && worker == nil && validationTask == nil && installer == nil && prepared == nil { startDownload(checkOnly: true) }
        case "install":
            if !busy && worker == nil && validationTask == nil && installer == nil {
                if let prepared { change(["phase": "ready", "message": "更新已下载，请保存工作并手动停止环境后安装", "retryable": true, "officialUpgrade": prepared.unsignedSource]) }
                else { startDownload(checkOnly: false) }
            }
        case "complete":
            if !busy && worker == nil && validationTask == nil && installer == nil && prepared != nil { beginInstallation() }
        case "cancel":
            if installer == nil && state["phase"] as? String != "installing" {
                generation += 1
                if let worker { CageUpdateFiles.stop(worker) }
                validationTask?.cancel()
                if worker == nil && validationTask == nil && prepared == nil { ownership = nil }
                change(["phase": prepared == nil ? "available" : "ready", "message": "更新已暂停，可稍后继续", "retryable": true])
            }
        default: break
        }
        return state
    }
    func cancelForTermination() {
        generation += 1
        if let worker { CageUpdateFiles.stop(worker) }
        validationTask?.cancel()
        try? installerInput?.fileHandleForWriting.close()
        if !installationCommitted, let installer { CageUpdateFiles.stop(installer) }
    }
    func prepareForTermination(_ completion: @escaping () -> Void) -> Bool {
        if installationCommitted { return false }
        if waitingForTermination { return true }
        let download = worker, validation = validationTask, helper = installer
        let directory = prepared?.directory
        cancelForTermination()
        guard download != nil || validation != nil || helper != nil || directory != nil else { return false }
        waitingForTermination = true
        change(["message": "正在取消更新并清理临时文件…"])
        DispatchQueue.global(qos: .utility).async {
            download?.waitUntilExit()
            validation?.wait()
            helper?.waitUntilExit()
            if let directory { try? FileManager.default.removeItem(at: directory) }
            DispatchQueue.main.async { self.waitingForTermination = false; completion() }
        }
        return true
    }
    var awaitingInitialWorkbench: Bool {
        guard let token = ProcessInfo.processInfo.environment["CAGE_UPDATE_CONFIRMATION"], UUID(uuidString: token) != nil else { return false }
        return !workbenchInitialized
    }
    func markLaunchReady() {
        workbenchInitialized = true
        guard !launchAcknowledged, let token = ProcessInfo.processInfo.environment["CAGE_UPDATE_CONFIRMATION"], UUID(uuidString: token) != nil,
              let data = try? JSONSerialization.data(withJSONObject: ["version": state["currentVersion"] as? String ?? ""]) else { return }
        do {
            try CageUpdateFiles.privateDirectory(root)
            try CageUpdateFiles.write(data, to: root.appendingPathComponent("launch-\(token).json"))
            launchAcknowledged = true
            change(["phase": "current", "message": "Cage 已更新至 \(state["currentVersion"] as? String ?? "")"])
        } catch { failure("更新已安装，但启动确认未能保存") }
    }
    private func change(_ values: [String: Any]) {
        if values["phase"] as? String == "current" {
            state.removeValue(forKey: "latestVersion")
            state["officialUpgrade"] = false
        } else if values["phase"] as? String == "manual" {
            state["officialUpgrade"] = false
        }
        for (key, value) in values { state[key] = value }
        onChange(state)
    }
    private func failure(_ message: String, retryAt: String = "") {
        if worker == nil && validationTask == nil && prepared == nil { ownership = nil }
        change(["phase": "failed", "message": message, "retryable": true, "retryAt": retryAt])
    }
    private var cage: URL { bundleURL.appendingPathComponent("Contents/Resources/cage") }
    private func resolveUpdatePublisher() -> (team: String, unsignedSource: Bool)? {
        if let ownTeam = try? readTeam() { return (ownTeam, false) }
        if canUpgradeUnsigned() { return (cageOfficialAppleTeamID, true) }
        return nil
    }
    private func startDownload(checkOnly: Bool) {
        do {
            if ownership == nil { ownership = try CageUpdateLock.worker(for: bundleURL, downloadsAt: checkOnly ? nil : root) }
        } catch {
            change(["phase": error is CageUpdateBusy ? "waiting" : "failed", "message": error.localizedDescription, "retryable": true])
            return
        }
        generation += 1; let operation = generation
        change(["phase": "checking", "message": "正在检查更新，优先使用主机网络", "receivedBytes": 0, "totalBytes": 0, "retryable": false, "retryAt": ""])
        // Signed installs pin strictly to their own Team ID. Eligible unsigned
        // source builds installed under /Applications or ~/Applications may
        // upgrade in-place to the pinned official signed release; all other
        // unsigned builds fall back to manual download without auto-replacing.
        DispatchQueue.global(qos: .utility).async {
            let resolved = self.resolveUpdatePublisher()
            let team = resolved?.team
            let unsignedSource = resolved?.unsignedSource ?? false
            DispatchQueue.main.async {
                guard self.generation == operation, !self.waitingForTermination else { return }
                let process = Process(); process.executableURL = self.cage; process.arguments = ["app-update", checkOnly || team == nil ? "check" : "download"]
                let pipe = Pipe(); process.standardOutput = pipe; process.standardError = FileHandle.nullDevice
                self.worker = process
                do { try process.run() } catch { self.worker = nil; self.ownership = nil; self.failure("无法启动更新检查，请确认应用组件完整"); return }
                DispatchQueue.global(qos: .utility).async {
                    var pending = Data(); var last: [String: Any]?
                    var lastVisible: [String: Any]?
                    while true {
                        let chunk = pipe.fileHandleForReading.availableData
                        if chunk.isEmpty { break }
                        pending.append(chunk)
                        if pending.count > 65536 { process.terminate(); break }
                        while let end = pending.firstIndex(of: 10) {
                            let line = pending.prefix(upTo: end); pending.removeSubrange(...end)
                            guard let value = try? JSONSerialization.jsonObject(with: line) as? [String: Any],
                                  let phase = value["phase"] as? String,
                                  ["checking", "current", "available", "downloading", "downloaded", "waiting", "failed"].contains(phase) else { continue }
                            last = value
                            var visible = value; visible.removeValue(forKey: "archivePath"); visible.removeValue(forKey: "sha256")
                            if team == nil && visible["phase"] as? String == "available" {
                                visible["phase"] = "manual"
                                visible["message"] = Self.unsignedManualMessage
                                visible["retryable"] = false
                                visible["retryAt"] = ""
                                visible["officialUpgrade"] = false
                            } else if visible["phase"] as? String == "current" {
                                visible["officialUpgrade"] = false
                            } else {
                                visible["officialUpgrade"] = unsignedSource
                            }
                            lastVisible = visible
                            if ["checking", "downloading", "downloaded"].contains(phase) {
                                DispatchQueue.main.async { if self.generation == operation { self.change(visible) } }
                            }
                        }
                    }
                    process.waitUntilExit()
                    let result = last, final = lastVisible
                    DispatchQueue.main.async {
                        if self.worker === process { self.worker = nil }
                        guard self.generation == operation else { self.ownership = nil; return }
                        if !checkOnly, team != nil, let result, result["phase"] as? String == "downloaded", process.terminationStatus == 0 {
                            self.verifyDownload(result, operation: operation)
                            return
                        }
                        self.ownership = nil
                        if process.terminationStatus != 0 {
                            self.failure(result?["message"] as? String ?? "更新请求未完成，请重试", retryAt: result?["retryAt"] as? String ?? "")
                        } else if let final, let phase = final["phase"] as? String,
                                  ["current", "manual", "waiting", "failed"].contains(phase) || (checkOnly && phase == "available") {
                            self.change(final)
                        } else {
                            self.failure("更新检查未返回完整结果，请重试")
                        }
                    }
                }
            }
        }
    }
    private func verifyDownload(_ result: [String: Any], operation: Int) {
        guard let path = result["archivePath"] as? String, let hash = result["sha256"] as? String,
              let version = result["latestVersion"] as? String, hash.range(of: "^[0-9a-f]{64}$", options: .regularExpression) != nil else { failure("下载进程返回了无效安装包"); return }
        let archive = URL(fileURLWithPath: path).resolvingSymlinksInPath()
        let downloadDirectory = archive.deletingLastPathComponent()
        guard downloadDirectory.deletingLastPathComponent().path == root.resolvingSymlinksInPath().path,
              downloadDirectory.lastPathComponent.hasPrefix("download-"), archive.lastPathComponent == "Cage.dmg" else { failure("安装包路径不属于本次私有下载"); return }
        let keepForRetry = headless && archive.path == CageUpdateFiles.cachedArchive(in: root).path
        let discardDownload = {
            if keepForRetry { CageUpdateFiles.discardCachedArchive(in: self.root) }
            else { try? FileManager.default.removeItem(at: downloadDirectory) }
        }
        change(["phase": "verifying", "message": "正在验证 Apple 签名和应用内容", "retryable": false])
        let task = CageUpdateValidation()
        validationTask = task
        DispatchQueue.global(qos: .userInitiated).async {
            defer {
                task.finish()
                DispatchQueue.main.async {
                    if self.validationTask === task { self.validationTask = nil }
                    if self.prepared == nil && self.worker == nil && self.validationTask == nil { self.ownership = nil }
                }
            }
            let directory = self.root.appendingPathComponent("verified-" + UUID().uuidString, isDirectory: true)
            do {
                try task.checkCancellation()
                guard let resolved = self.resolveUpdatePublisher() else {
                    discardDownload()
                    DispatchQueue.main.async {
                        if self.generation == operation {
                            self.change(["phase": "manual", "message": Self.unsignedManualMessage, "retryable": false, "retryAt": ""])
                        }
                    }
                    return
                }
                let team = resolved.team
                let unsignedSource = resolved.unsignedSource
                try CageUpdateFiles.privateDirectory(directory)
                let app = try CageUpdateFiles.extract(archive: archive, hash: hash, version: version, team: team, directory: directory, validation: task)
                try task.checkCancellation()
                if !keepForRetry { discardDownload() }
                DispatchQueue.main.async {
                    if self.generation != operation { try? FileManager.default.removeItem(at: directory); return }
                    self.prepared = (directory, app, version, team, unsignedSource)
                    if self.validationTask === task { self.validationTask = nil }
                    self.change(["phase": "ready", "message": "更新已下载，请保存工作并手动停止环境后安装", "retryable": true, "officialUpgrade": unsignedSource])
                }
            } catch {
                try? FileManager.default.removeItem(at: directory)
                if !keepForRetry || !task.isCancelled { discardDownload() }
                DispatchQueue.main.async { if self.generation == operation { self.failure(error.localizedDescription) } }
            }
        }
    }
    private func beginInstallation() {
        guard let prepared, installer == nil else { return }
        if CageUpdateProcess.hasOtherWindow(bundleURL) {
            change(["phase": "waiting", "message": "此安装还有其他 Cage 工作台。请保存工作并退出其他窗口后重试更新。", "retryable": true])
            return
        }
        change(["phase": "installing", "message": "正在准备安装更新", "retryable": false, "officialUpgrade": prepared.unsignedSource])
        let operation = generation
        let task = CageUpdateValidation()
        validationTask = task
        DispatchQueue.global(qos: .userInitiated).async {
            defer {
                task.finish()
                DispatchQueue.main.async { if self.validationTask === task { self.validationTask = nil } }
            }
            do {
                if prepared.unsignedSource {
                    guard prepared.team == cageOfficialAppleTeamID,
                          CageUpdateFiles.isInstalledApplicationsPath(self.bundleURL) else {
                        throw CageUpdateError("请先将 Cage 安装到应用程序目录，再自动切换为官方签名版本")
                    }
                    try CageUpdateFiles.verifyUnsignedAppIdentity(self.bundleURL)
                } else {
                    guard !self.bundleURL.path.hasPrefix("/Volumes/"), self.bundleURL.pathExtension == "app" else {
                        throw CageUpdateError("请先将 Cage 安装到应用程序目录，再自动更新")
                    }
                }
                do { try CageUpdateFiles.verifyApp(prepared.app, version: prepared.version, team: prepared.team, validation: task) }
                catch {
                    if self.headless && !task.isCancelled { CageUpdateFiles.discardCachedArchive(in: self.root) }
                    throw error
                }
                let helper = prepared.directory.appendingPathComponent("CageUpdateInstaller")
                let originalHelper = self.bundleURL.appendingPathComponent("Contents/Resources/CageUpdateInstaller")
                try? FileManager.default.removeItem(at: helper)
                try FileManager.default.copyItem(at: originalHelper, to: helper)
                if !prepared.unsignedSource {
                    _ = try CageUpdateFiles.signingInfo(helper, bundle: false, expectedTeam: prepared.team)
                }
                let plan = CageUpdatePlan(version: prepared.version, team: prepared.team, target: self.bundleURL.path, candidate: prepared.app.path,
                    backup: prepared.directory.appendingPathComponent("previous.app").path, parentPID: ProcessInfo.processInfo.processIdentifier,
                    resultPath: self.root.appendingPathComponent("last-result.json").path, unsignedSource: prepared.unsignedSource, requiresCommitReceipt: self.headless)
                let planURL = prepared.directory.appendingPathComponent("install.json")
                try CageUpdateFiles.write(JSONEncoder().encode(plan), to: planURL)
                try task.checkCancellation()
                DispatchQueue.main.async { if self.generation == operation { self.launchInstaller(helper: helper, plan: planURL) } }
            } catch { DispatchQueue.main.async { if self.generation == operation { self.failure(error.localizedDescription) } } }
        }
    }
    private func launchInstaller(helper: URL, plan: URL) {
        let process = Process(); process.executableURL = helper; process.arguments = [plan.path]
        let input = Pipe(), output = Pipe(); process.standardInput = input; process.standardOutput = output; process.standardError = FileHandle.nullDevice
        installer = process; installerInput = input
        do { try process.run() } catch { installer = nil; installerInput = nil; failure("无法启动安全安装程序"); return }
        DispatchQueue.global(qos: .userInitiated).async {
            let response = CageUpdateFiles.installerReply(from: output.fileHandleForReading)
            DispatchQueue.main.async {
                guard self.installer === process, !self.waitingForTermination else { return }
                if response?["status"] as? String == "ready" && (!self.headless || response?["canConfirmCommit"] as? Bool == true) { self.prepareEnvironment() }
                else { self.abortInstaller(); self.failure(response?["message"] as? String ?? "安全安装准备失败，原应用已保留") }
            }
        }
    }
    private func awaitCommitReceipt(from process: Process) {
        DispatchQueue.global(qos: .userInitiated).async {
            let output = (process.standardOutput as? Pipe)?.fileHandleForReading
            let reply = output.flatMap { CageUpdateFiles.installerReply(from: $0, within: 15) }
            DispatchQueue.main.async {
                if reply?["status"] as? String == "committed" {
                    self.change(["phase": "handed_off", "message": "安全安装程序已接手，将启动新版 Cage 核验后完成安装"])
                    self.quit()
                } else {
                    self.failure("安装请求已发送，但未收到安全安装程序接手的确认。请核对安装状态和当前版本。")
                }
            }
        }
    }
    private func abortInstaller() {
        try? installerInput?.fileHandleForWriting.close()
        installerInput = nil
        if let helper = installer {
            CageUpdateFiles.stop(helper)
            DispatchQueue.global(qos: .utility).async {
                helper.waitUntilExit()
                DispatchQueue.main.async { if self.installer === helper { self.installer = nil } }
            }
        }
    }
    private func prepareEnvironment() {
        change(["phase": "installing", "message": "正在确认环境已停止"])
        DispatchQueue.global(qos: .userInitiated).async {
            do {
                let arguments = ["prepare-update", "--json"]
                let process = Process(); process.executableURL = self.cage; process.arguments = arguments
                let output = Pipe(); process.standardOutput = output; process.standardError = FileHandle.nullDevice
                try process.run()
                let expiration = DispatchWorkItem { if process.isRunning { process.terminate() } }
                DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + 180, execute: expiration)
                let data = output.fileHandleForReading.readDataToEndOfFile(); process.waitUntilExit(); expiration.cancel()
                guard data.count <= 16384, let result = try JSONSerialization.jsonObject(with: data) as? [String: Any] else { throw CageUpdateError("无法确认环境已停止") }
                DispatchQueue.main.async {
                    guard !self.waitingForTermination else { return }
                    switch result["status"] as? String {
                    case "prepared" where process.terminationStatus == 0:
                        guard let installer = self.installer, installer.isRunning, let input = self.installerInput else { self.failure("安装程序已退出，原应用已保留"); self.reconnect(); return }
                        do {
                            try input.fileHandleForWriting.write(contentsOf: Data("COMMIT\n".utf8))
                            try input.fileHandleForWriting.close()
                            self.installationCommitted = true
                            if self.headless { self.awaitCommitReceipt(from: installer) }
                            else {
                                self.change(["phase": "installing", "message": "正在安装并重启 Cage"])
                                self.quit()
                            }
                        } catch { self.abortInstaller(); self.failure("无法开始安装，原应用已保留"); self.reconnect() }
                    case "stop_required":
                        self.abortInstaller()
                        let reason = result["message"] as? String ?? "环境仍在运行"
                        if self.headless { self.change(["phase": "waiting", "message": reason + "。保存工作后运行 cage stop，再重试 cage update。"]) }
                        else { self.failure(reason + "。请在工作台停止环境后重试安装") }
                    default:
                        self.abortInstaller(); self.failure(result["message"] as? String ?? "环境未能安全停止，更新未安装")
                    }
                }
            } catch { DispatchQueue.main.async { self.abortInstaller(); self.failure(error.localizedDescription) } }
        }
    }
}
