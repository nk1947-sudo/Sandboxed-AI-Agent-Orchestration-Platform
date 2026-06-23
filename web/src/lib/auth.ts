// Auth state. There is NO token in browser memory or storage — the session
// lives in an HttpOnly cookie the browser sends automatically. We only cache the
// current user object (id/username/role) so the UI can render it; a page refresh
// re-derives it from the server via restoreSession().
import {
  me as apiMe,
  login as apiLogin,
  logout as apiLogout,
  User,
} from "./api";

let _user: User | null = null;

export function currentUser(): User | null {
  return _user;
}

export function isAuthenticated(): boolean {
  return _user !== null;
}

// restoreSession asks the server who we are using the session cookie. Returns
// the user when a valid session exists (survives page refresh), else null.
export async function restoreSession(): Promise<User | null> {
  try {
    _user = await apiMe();
  } catch {
    _user = null;
  }
  return _user;
}

export async function login(username: string, password: string): Promise<User> {
  const u = await apiLogin(username, password);
  _user = u;
  return u;
}

export async function logout(): Promise<void> {
  try {
    await apiLogout();
  } finally {
    _user = null;
  }
}
