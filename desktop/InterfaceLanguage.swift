import Foundation

enum CageLanguage {
    private struct Catalog: Decodable {
        let messages: [String: String]
    }
    // Startup and native preference refresh run on the application thread.
    private static var messages: [String: String] = [:]

    static func refresh() {
        guard let executable = Bundle.main.url(forResource: "cage", withExtension: nil) else { return }
        let process = Process()
        process.executableURL = executable
        process.arguments = ["interface-language", "--json"]
        let output = Pipe()
        process.standardOutput = output
        process.standardError = FileHandle.nullDevice
        do {
            try process.run()
            let data = output.fileHandleForReading.readDataToEndOfFile()
            process.waitUntilExit()
            if process.terminationStatus == 0 {
                messages = try JSONDecoder().decode(Catalog.self, from: data).messages
            }
        } catch {
            // An incomplete installation can still display the recovery UI.
        }
    }

    static func text(_ source: String) -> String { messages[source] ?? source }
    // Translations are whole sentences; only the {0} detail is inserted, so word order, spacing and punctuation stay per-language.
    static func format(_ source: String, _ detail: String) -> String { text(source).replacingOccurrences(of: "{0}", with: detail) }
}
