import Foundation

/// Something that can greet.
protocol Greeting {
    associatedtype Output
    /// Says hello.
    func greet(_ who: String, times: Int) -> String
    var name: String { get }
}

/// Greets people.
@MainActor
public final class Greeter: NSObject, Greeting {
    typealias Output = String

    /// Who to greet by default.
    @objc dynamic var name: String = "world"
    private(set) var count = 0 // greetings so far
    static let shared = Greeter(name: "shared")

    /// The greeting text.
    var greeting: String {
        get { "Hello, \(name)!" }
        set { name = newValue }
    }

    init(name: String) {
        self.name = name
        super.init()
    }

    convenience override init() {
        self.init(name: "world")
    }

    deinit {
        print("bye")
    }

    /* Greets someone. */
    @objc func greet(_ who: String, times: Int = 1) -> String {
        count += times
        return who
    }

    subscript(index: Int) -> Character {
        return Array(self.name)[index]
    }

    struct Config {
        var loud: Bool
        enum Level { case low, high }
    }
}

extension Greeter {
    /// Waves.
    func wave(at target: Greeter) async throws -> Bool {
        return true
    }

    var shout: String { greeting.uppercased() }
}

extension String {
    func shouted() -> String { uppercased() }
}
