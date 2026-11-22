import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable, map, switchMap } from 'rxjs';
import type { TokenPair } from './auth.service';

export const PASSKEY_API = '/api/passkeys';

export interface Passkey {
  id: string;
  name: string;
  created_at: string;
  last_used_at: string | null;
}

interface PasskeyList {
  items: Passkey[];
  can_manage: boolean;
}

export function supportsPasskeys(): boolean {
  return globalThis.isSecureContext &&
    typeof PublicKeyCredential !== 'undefined' &&
    typeof PublicKeyCredential.parseCreationOptionsFromJSON === 'function' &&
    typeof PublicKeyCredential.parseRequestOptionsFromJSON === 'function' &&
    typeof PublicKeyCredential.prototype.toJSON === 'function';
}

@Injectable({ providedIn: 'root' })
export class PasskeyService {
  private readonly http = inject(HttpClient);

  available(): Observable<boolean> {
    return this.http.get<{ enabled: boolean }>(PASSKEY_API + '/status').pipe(map((r) => r.enabled));
  }

  list(): Observable<PasskeyList> {
    return this.http.get<PasskeyList>(PASSKEY_API);
  }

  register(name: string, signal: AbortSignal): Observable<Passkey> {
    return this.http.post<{
      publicKey: PublicKeyCredentialCreationOptionsJSON;
      ceremony_token: string;
    }>(PASSKEY_API + '/register/begin', {}).pipe(
      switchMap(async (options) => {
        const credential = await navigator.credentials.create({
          publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(options.publicKey),
          signal,
        });
        if (!(credential instanceof PublicKeyCredential)) {
          throw new DOMException('No passkey was created', 'NotAllowedError');
        }
        return { credential: credential.toJSON(), name, ceremony_token: options.ceremony_token };
      }),
      switchMap((body) => this.http.post<Passkey>(PASSKEY_API + '/register/finish', body)),
    );
  }

  login(signal: AbortSignal): Observable<TokenPair> {
    return this.http.post<{
      publicKey: PublicKeyCredentialRequestOptionsJSON;
    }>(PASSKEY_API + '/login/begin', {}).pipe(
      switchMap(async ({ publicKey }) => {
        const credential = await navigator.credentials.get({
          publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(publicKey),
          signal,
        });
        if (!(credential instanceof PublicKeyCredential)) {
          throw new DOMException('No passkey was selected', 'NotAllowedError');
        }
        return { credential: credential.toJSON() };
      }),
      switchMap((body) => this.http.post<TokenPair>(PASSKEY_API + '/login/finish', body)),
    );
  }

  remove(id: string): Observable<void> {
    return this.http.delete<void>(PASSKEY_API + '/' + encodeURIComponent(id));
  }
}
