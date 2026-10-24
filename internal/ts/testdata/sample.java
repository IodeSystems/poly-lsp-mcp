package app;

import java.util.List;

/** Doc. */
public class W<T> implements Runnable {
    private final List<T> items = List.of();

    @Override
    public void run() {
        for (T t : items) {
            System.out.println(t);
        }
    }
}
