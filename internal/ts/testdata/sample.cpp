#include <string>

namespace app {
struct Pair { int x, y; };

class W {
 public:
  std::string raw() const { return R"x(a "b" c)x"; }
};

int App::counter = 5;
}  // namespace app
