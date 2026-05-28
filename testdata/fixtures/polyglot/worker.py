"""Background worker that processes UserID values."""

UserID = int


def process(user_id: UserID) -> str:
    return f"processed {user_id}"
