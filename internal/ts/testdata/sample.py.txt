"""Module doc."""
import os


class A:
    def f(self, x: int) -> str:
        if x:
            return f"{x!r} and {os.sep}"
        # comment
        return ""


def g(*args, **kw):
    return [a for a in args if a]
