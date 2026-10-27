import UIKit

/// The first screen.
final class ViewController: UIViewController {
    @IBOutlet weak var label: UILabel!

    override func viewDidLoad() {
        super.viewDidLoad()
        label.text = makeGreeter(named: "iOS").greet("you")
    }

    @IBAction func tap(_ sender: Any) {
        label.text = Direction.north.opposite.rawValue
    }
}
