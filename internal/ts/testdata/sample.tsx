import { useState } from "react";

// doc
export function Card(props: { id?: string }) {
  const [v, setV] = useState<string | null>(null)
  const t = `id ${props.id} \n done`
  const ok = v ? v.length : 0
  let n = 1
  n++
  return <div className="c">{t}{ok}</div>
}

type F = ({ a }: { a: number }) => number;
