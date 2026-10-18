/**
 * Where a share page parks the address an anonymous visitor should land on
 * once they have signed in.
 *
 * It lived in ShareView because that is where it is written, and App imported
 * it from there to read it back. That import is what stops ShareView from
 * being lazy: a static import of the constant drags the whole page into the
 * initial bundle, for the benefit of one string. A sessionStorage key is not
 * share-page behaviour, so it does not belong in the share page.
 */
export const POST_LOGIN_REDIRECT = 'yuar:postLoginRedirect'