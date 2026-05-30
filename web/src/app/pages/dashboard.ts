import { HttpClient } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';
import { AuthService, Profile } from '../core/auth.service';

interface Business {
  id: string;
  name: string;
  tenant_root_id: string;
  is_tenant_root: boolean;
  status: string;
}

@Component({
  selector: 'app-dashboard',
  imports: [FormsModule],
  template: `
    <section class="card">
      <div class="spread">
        <div>
          <h1>Your businesses</h1>
          @if (profile(); as p) {
            <p class="profile">Signed in as <b>{{ p.display_name }}</b> ({{ p.email }})</p>
          }
        </div>
        <button class="ghost" style="width:auto;margin:0;padding:8px 14px" (click)="logout()">Sign out</button>
      </div>

      <ul class="biz-list">
        @for (b of businesses(); track b.id) {
          <li class="biz">
            <span class="name">{{ b.name }}</span>
            @if (b.is_tenant_root) { <span class="pill">master</span> }
          </li>
        } @empty {
          <li class="empty">No businesses yet — create your master business below.</li>
        }
      </ul>
    </section>

    <section class="card" style="margin-top:20px">
      <h2>Create a master business</h2>
      <form (ngSubmit)="create()">
        <label for="bizname">Business name</label>
        <input id="bizname" type="text" name="name" [(ngModel)]="name" placeholder="Acme, Inc." required />
        <button type="submit" [disabled]="creating()">{{ creating() ? 'Creating…' : 'Create business' }}</button>
      </form>
      @if (error()) { <p class="msg error">{{ error() }}</p> }
    </section>
  `,
})
export class DashboardComponent implements OnInit {
  private auth = inject(AuthService);
  private http = inject(HttpClient);
  private router = inject(Router);

  profile = signal<Profile | null>(null);
  businesses = signal<Business[]>([]);
  name = '';
  creating = signal(false);
  error = signal('');

  ngOnInit(): void {
    this.auth.me().subscribe({ next: (p) => this.profile.set(p), error: () => this.forceLogin() });
    this.loadBusinesses();
  }

  loadBusinesses(): void {
    this.http.get<{ items: Business[] }>('/api/v1/businesses').subscribe({
      next: (r) => this.businesses.set(r.items ?? []),
      error: () => this.error.set('Could not load businesses.'),
    });
  }

  create(): void {
    if (!this.name.trim()) {
      return;
    }
    this.creating.set(true);
    this.error.set('');
    this.http.post('/api/v1/businesses', { name: this.name }).subscribe({
      next: () => {
        this.creating.set(false);
        this.name = '';
        this.loadBusinesses();
      },
      error: () => {
        this.creating.set(false);
        this.error.set('Could not create the business. Is your email verified?');
      },
    });
  }

  logout(): void {
    this.auth.logout().subscribe({ next: () => this.forceLogin(), error: () => this.forceLogin() });
  }

  private forceLogin(): void {
    void this.router.navigateByUrl('/login');
  }
}
