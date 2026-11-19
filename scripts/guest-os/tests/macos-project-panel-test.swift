import Foundation

// The panel source also contains the live attestation and capture functions.
struct Failure: Error {
    let code: Int32
    let message: String
    init(_ code: Int32, _ message: String) { self.code = code; self.message = message }
}

@main
struct ProjectPanelSearchTest {
    final class Element {
        let name: String
        var children: [Element]
        init(_ name: String, _ children: [Element] = []) { self.name = name; self.children = children }
    }

    static func find(_ root: Element) -> Element? {
        findProjectPanelElement(root, children: { $0.children }, matching: { $0.name == "Use Project" })
    }

    static func main() {
        let button = Element("Use Project")
        let fileList = Element("file list", (0..<3000).map { Element("file \($0)") })
        let panel = Element("panel", [Element("content", [fileList]), Element("actions", [Element("Cancel"), button])])
        precondition(find(panel) === button, "file rows exhausted the search before the approval button")
        precondition(find(Element("panel", [Element("Cancel")])) == nil)
        var deep = button
        for _ in 0..<15 { deep = Element("container", [deep]) }
        precondition(find(deep) == nil, "search exceeded the existing depth bound")
        let cycle = Element("cycle")
        cycle.children = [cycle]
        var visits = 0
        let absent = findProjectPanelElement(cycle, children: { $0.children }, matching: { _ in visits += 1; return false })
        precondition(absent == nil && visits == 15, "cyclic AX children escaped the depth bound")
        cycle.children = []
        visits = 0
        let crowded = findProjectPanelElement(fileList, children: { $0.children }, matching: { _ in visits += 1; return false })
        precondition(crowded == nil && visits == 2048, "search failed to retain the node bound")
        print("Project approval search: crowded file list, missing button, depth bound and node bound passed")
    }
}
