export type UserID = number;

export async function fetchUser(id: UserID): Promise<string> {
  const res = await fetch(`/users/${id}`);
  return res.text();
}
