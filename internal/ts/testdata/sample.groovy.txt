package com.example

import groovy.transform.CompileStatic

String greeting = "hi"

class Widget {
    static final int MAX = 10

    def scroll(boolean up, int keys = 0) {
        println "scroll ${up}"
    }
}
