#include <stdio.h>
#define MAX 10

struct pair { int x, y; };

static int add(int a, int b) { return a + b; }

int main(void) {
    struct pair p = {1, 2};
    printf("%d\n", add(p.x, p.y));
    return 0;
}
