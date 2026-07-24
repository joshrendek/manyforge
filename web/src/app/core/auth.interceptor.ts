import { HttpErrorResponse, HttpInterceptorFn } from '@angular/common/http';
import { inject } from '@angular/core';
import { Router } from '@angular/router';
import { catchError, switchMap, throwError } from 'rxjs';
import { AuthService } from './auth.service';

// Attaches the bearer access token and transparently recovers from an expired
// one: on a 401, it refreshes the token pair (single-flight, see AuthService)
// and retries the original request once. A failed refresh clears the session and
// sends the user to the login screen.
export const authInterceptor: HttpInterceptorFn = (req, next) => {
  // inject() must run in the interceptor's injection context — not inside the
  // async catchError callback below — so resolve dependencies up front.
  const auth = inject(AuthService);
  const router = inject(Router);

  const token = localStorage.getItem('mf_access');
  const authReq = token ? req.clone({ setHeaders: { Authorization: `Bearer ${token}` } }) : req;

  // Never try to refresh for:
  //  - the auth endpoints themselves (login/signup/refresh/…) — a 401 there is a real
  //    failure and refreshing would loop; and
  //  - the public feedback ingress (/feedback/public/…) — it is principal-less (keyed by a
  //    publishable board key), so a 401 means an unknown/revoked key or a private board and
  //    an anonymous portal visitor must NOT be bounced to /login.
  const skipRefresh =
    req.url.includes('/api/v1/auth/') || req.url.includes('/api/v1/feedback/public/');

  return next(authReq).pipe(
    catchError((err: HttpErrorResponse) => {
      if (err.status !== 401 || skipRefresh) {
        return throwError(() => err);
      }
      return auth.refreshAccessToken().pipe(
        // Retry via next() (not the whole chain) with the fresh token, so the
        // retry can't re-enter this interceptor and loop.
        switchMap((newToken) =>
          next(req.clone({ setHeaders: { Authorization: `Bearer ${newToken}` } })),
        ),
        catchError((refreshErr) => {
          auth.clearSession();
          void router.navigateByUrl('/login');
          return throwError(() => refreshErr);
        }),
      );
    }),
  );
};
