// Token is stored in module-level memory only — never written to localStorage,
// sessionStorage, or cookies — to reduce XSS blast radius.
let _token = "";

export function getToken(): string {
  return _token;
}

export function setToken(t: string): void {
  _token = t;
}

export function clearToken(): void {
  _token = "";
}

export function isAuthenticated(): boolean {
  return _token.length > 0;
}
