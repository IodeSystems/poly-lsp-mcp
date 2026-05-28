-- Initial users schema. The UserID column is the cross-language
-- identifier that go/ts/py all read and write.

CREATE TABLE users (
  UserID BIGINT PRIMARY KEY,
  email TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX users_email_idx ON users (email);
CREATE INDEX users_UserID_idx ON users (UserID);
