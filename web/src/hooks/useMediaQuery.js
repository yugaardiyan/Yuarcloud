import { useEffect, useState } from 'react'

/**
 * Live result of a CSS media query.
 *
 * Used where the same data has to be laid out differently rather than merely
 * styled differently — the settings tables do not shrink into a phone, they
 * stop being tables. Reading the breakpoint in JS keeps those two layouts in
 * the markup where each can be written properly, instead of turning a table
 * into blocks with `!important` over the inline styles the cells already carry.
 *
 * The initial value is read synchronously so the first paint is already the
 * right one; a matchMedia listener keeps it honest across a rotation.
 */
export function useMediaQuery(query) {
  const [matches, setMatches] = useState(() =>
    typeof window !== 'undefined' && window.matchMedia(query).matches)

  useEffect(() => {
    const mq = window.matchMedia(query)
    const update = () => setMatches(mq.matches)
    update()
    mq.addEventListener('change', update)
    return () => mq.removeEventListener('change', update)
  }, [query])

  return matches
}
