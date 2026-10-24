package app

import a.B
import c.D as E

/* outer /* nested */ comment */
fun String.shout(): String = this.uppercase()

class Widget(val name: String) {
    val greeting = "hi $name, ${name.length} chars \$literal"
    val block = """
        raw "quoted" $name
    """
    fun f(x: Int?): Int {
        val y = x
            ?.plus(1)
            ?: 0
        return y
    }
}
