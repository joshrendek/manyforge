import { HttpInterceptorFn } from '@angular/common/http';

// Attaches the bearer access token. Reads localStorage directly (not AuthService)
// to avoid an HttpClient<->AuthService DI cycle.
export const authInterceptor: HttpInterceptorFn = (req, next) => {
  const token = localStorage.getItem('mf_access');
  if (token) {
    req = req.clone({ setHeaders: { Authorization: `Bearer ${token}` } });
  }
  return next(req);
};
