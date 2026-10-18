/**
 * Run a context mutation whose failure is already reported.
 *
 * removeItems, moveItems, copyItems, restoreItems and uploadFiles all toast the
 * error and then re-throw, so a caller that needs to branch on failure can.
 * Every interactive caller here cannot — an onClick handler, a drop handler,
 * a confirm button — and calling one of those without a catch made the browser
 * log "Uncaught (in promise)" on every failure, including the ordinary ones
 * like a full disk or a name that already exists. The toast already says it;
 * the rejection only duplicates it somewhere nobody looks, and a global error
 * reporter configured for real faults ends up reporting user-visible
 * non-faults.
 *
 * `void` so the intent is visible at the call site rather than implied.
 */
export function fireAndForget(promise) {
  if (promise && typeof promise.then === 'function') {
    promise.catch(() => {})
  }
}