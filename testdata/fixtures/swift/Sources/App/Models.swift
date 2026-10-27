import Foundation

/// A compass direction.
enum Direction: String, CaseIterable {
    case north, south
    case east = "E"
    /// Somewhere else.
    case other

    var opposite: Direction {
        switch self {
        case .north: return .south
        default: return self
        }
    }
}

enum Shape {
    case circle(radius: Double)
    case rect(w: Double, h: Double)
}

struct Point: Equatable {
    var x: Double
    var y: Double

    static func + (lhs: Point, rhs: Point) -> Point { Point(x: 1, y: 2) }

    mutating func move(by delta: Point) {
        self = self + delta
    }
}

typealias Callback = (Int) -> Void

let origin = Point(x: 0, y: 0)

func makeGreeter(named name: String) -> Greeter {
    return Greeter(name: name)
}

actor Counter {
    var value = 0
    func increment() -> Int {
        value += 1
        return value
    }
}
