import { HttpClient, HttpParams } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';
import { Page } from './ticket.service';

export type MailingListStatus = 'active' | 'archived';
export type SubscriberStatus = 'pending' | 'active' | 'unsubscribed' | 'bounced' | 'complained';
export type MailingSendingMode = 'relay' | 'resend' | 'ses';
export type MailingSendingProfileStatus = 'unverified' | 'verified' | 'error';

export interface MailingList {
  id: string;
  business_id: string;
  tenant_root_id: string;
  slug: string;
  name: string;
  description: string | null;
  double_opt_in: boolean;
  status: MailingListStatus;
  created_at: string;
  updated_at: string;
}

export interface MailingSubscriber {
  id: string;
  business_id: string;
  tenant_root_id: string;
  list_id: string;
  email: string;
  first_name: string | null;
  last_name: string | null;
  attributes: Record<string, unknown>;
  status: SubscriberStatus;
  contact_id: string | null;
  consent_source: 'public_form' | 'api' | 'csv_import' | 'crm' | 'manual';
  consent_attested_by: string | null;
  consent_at: string;
  confirmed_at: string | null;
  unsubscribed_at: string | null;
  status_reason: string | null;
  tags: string[];
  created_at: string;
  updated_at: string;
}

export interface MailingListKey {
  id: string;
  business_id: string;
  tenant_root_id: string;
  list_id: string;
  publishable_key: string;
  label: string | null;
  status: 'enabled' | 'revoked';
  has_secret: boolean;
  secret?: string;
  created_at: string;
  revoked_at: string | null;
}

export interface MailingTemplate {
  id: string;
  business_id: string;
  tenant_root_id: string;
  name: string;
  subject: string;
  preheader: string | null;
  body_markdown: string;
  track_opens: boolean;
  track_clicks: boolean;
  created_at: string;
  updated_at: string;
}

export interface MailingImportResult {
  imported: number;
  skipped: number;
  errors: Array<{ row: number; message: string }>;
}

export interface MailingSendingProfile {
  id: string;
  business_id: string;
  tenant_root_id: string;
  mode: MailingSendingMode;
  from_email: string;
  from_name: string;
  reply_to: string | null;
  postal_address: string | null;
  email_domain_id: string | null;
  ses_region: string | null;
  ses_configuration_set: string | null;
  sns_topic_arn: string | null;
  status: MailingSendingProfileStatus;
  last_verified_at: string | null;
  verify_error: string | null;
  has_credentials: boolean;
  created_at: string;
  updated_at: string;
}

export interface MailingSendingProfileInput {
  mode: MailingSendingMode;
  from_email: string;
  from_name: string;
  reply_to?: string | null;
  postal_address?: string | null;
  email_domain_id?: string | null;
  resend?: { api_key: string; webhook_secret?: string };
  ses?: { access_key_id: string; secret_access_key: string };
  ses_region?: string | null;
  ses_configuration_set?: string | null;
  sns_topic_arn?: string | null;
}

export interface SubscriberFilters {
  q?: string;
  status?: SubscriberStatus;
  tag?: string;
  cursor?: string;
  limit?: number;
}

export interface CreateSubscriber {
  email: string;
  first_name?: string | null;
  last_name?: string | null;
  attributes?: Record<string, unknown>;
  tags?: string[];
  skip_confirmation?: boolean;
}

@Injectable({ providedIn: 'root' })
export class MailingService {
  private http = inject(HttpClient);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}/mailing`;
  }

  listLists(businessId: string, cursor?: string): Observable<Page<MailingList>> {
    const params = cursor ? new HttpParams().set('cursor', cursor) : undefined;
    return this.http.get<Page<MailingList>>(`${this.base(businessId)}/lists`, { params });
  }

  getList(businessId: string, listId: string): Observable<MailingList> {
    return this.http.get<MailingList>(`${this.base(businessId)}/lists/${listId}`);
  }

  createList(
    businessId: string,
    body: { name: string; slug?: string; description?: string | null; double_opt_in?: boolean },
  ): Observable<MailingList> {
    return this.http.post<MailingList>(`${this.base(businessId)}/lists`, body);
  }

  updateList(
    businessId: string,
    listId: string,
    body: Partial<{ name: string; description: string | null; double_opt_in: boolean }>,
  ): Observable<MailingList> {
    return this.http.patch<MailingList>(`${this.base(businessId)}/lists/${listId}`, body);
  }

  archiveList(businessId: string, listId: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/lists/${listId}`);
  }

  listSubscribers(
    businessId: string,
    listId: string,
    filters: SubscriberFilters = {},
  ): Observable<Page<MailingSubscriber>> {
    let params = new HttpParams();
    if (filters.q) params = params.set('q', filters.q);
    if (filters.status) params = params.set('status', filters.status);
    if (filters.tag) params = params.set('tag', filters.tag);
    if (filters.cursor) params = params.set('cursor', filters.cursor);
    if (filters.limit != null) params = params.set('limit', String(filters.limit));
    return this.http.get<Page<MailingSubscriber>>(
      `${this.base(businessId)}/lists/${listId}/subscribers`,
      { params },
    );
  }

  createSubscriber(
    businessId: string,
    listId: string,
    body: CreateSubscriber,
  ): Observable<MailingSubscriber> {
    return this.http.post<MailingSubscriber>(
      `${this.base(businessId)}/lists/${listId}/subscribers`,
      body,
    );
  }

  updateSubscriber(
    businessId: string,
    listId: string,
    subscriberId: string,
    body: Partial<{
      first_name: string | null;
      last_name: string | null;
      attributes: Record<string, unknown>;
      tags: string[];
      status: SubscriberStatus;
      status_reason: string | null;
    }>,
  ): Observable<MailingSubscriber> {
    return this.http.patch<MailingSubscriber>(
      `${this.base(businessId)}/lists/${listId}/subscribers/${subscriberId}`,
      body,
    );
  }

  unsubscribeSubscriber(
    businessId: string,
    listId: string,
    subscriberId: string,
  ): Observable<void> {
    return this.http.delete<void>(
      `${this.base(businessId)}/lists/${listId}/subscribers/${subscriberId}`,
    );
  }

  addFromContacts(
    businessId: string,
    listId: string,
    contactIds: string[],
    skipConfirmation = false,
  ): Observable<MailingImportResult> {
    return this.http.post<MailingImportResult>(
      `${this.base(businessId)}/lists/${listId}/subscribers/from-contacts`,
      { contact_ids: contactIds, skip_confirmation: skipConfirmation },
    );
  }

  importSubscribers(
    businessId: string,
    listId: string,
    file: File,
    consentAttested: boolean,
    skipConfirmation = false,
  ): Observable<MailingImportResult> {
    const form = new FormData();
    form.append('file', file);
    form.append('consent_attested', String(consentAttested));
    form.append('skip_confirmation', String(skipConfirmation));
    return this.http.post<MailingImportResult>(
      `${this.base(businessId)}/lists/${listId}/subscribers/import`,
      form,
    );
  }

  exportSubscribers(businessId: string, listId: string): Observable<Blob> {
    return this.http.get(`${this.base(businessId)}/lists/${listId}/subscribers/export`, {
      responseType: 'blob',
    });
  }

  listKeys(businessId: string, listId: string): Observable<{ items: MailingListKey[] }> {
    return this.http.get<{ items: MailingListKey[] }>(
      `${this.base(businessId)}/lists/${listId}/keys`,
    );
  }

  createKey(businessId: string, listId: string, label?: string): Observable<MailingListKey> {
    return this.http.post<MailingListKey>(`${this.base(businessId)}/lists/${listId}/keys`, {
      label: label?.trim() || null,
    });
  }

  revokeKey(businessId: string, listId: string, keyId: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/lists/${listId}/keys/${keyId}`);
  }

  getSendingProfile(businessId: string): Observable<MailingSendingProfile> {
    return this.http.get<MailingSendingProfile>(`${this.base(businessId)}/sending-profile`);
  }

  putSendingProfile(
    businessId: string,
    body: MailingSendingProfileInput,
  ): Observable<MailingSendingProfile> {
    return this.http.put<MailingSendingProfile>(`${this.base(businessId)}/sending-profile`, body);
  }

  deleteSendingProfile(businessId: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/sending-profile`);
  }

  verifySendingProfile(businessId: string): Observable<MailingSendingProfile> {
    return this.http.post<MailingSendingProfile>(
      `${this.base(businessId)}/sending-profile/verify`,
      {},
    );
  }

  testSendingProfile(businessId: string, to: string): Observable<void> {
    return this.http.post<void>(`${this.base(businessId)}/sending-profile/test-send`, { to });
  }

  listTemplates(businessId: string, cursor?: string): Observable<Page<MailingTemplate>> {
    const params = cursor ? new HttpParams().set('cursor', cursor) : undefined;
    return this.http.get<Page<MailingTemplate>>(`${this.base(businessId)}/templates`, { params });
  }

  getTemplate(businessId: string, templateId: string): Observable<MailingTemplate> {
    return this.http.get<MailingTemplate>(`${this.base(businessId)}/templates/${templateId}`);
  }

  createTemplate(
    businessId: string,
    body: {
      name: string;
      subject: string;
      preheader?: string | null;
      body_markdown: string;
      track_opens?: boolean;
      track_clicks?: boolean;
    },
  ): Observable<MailingTemplate> {
    return this.http.post<MailingTemplate>(`${this.base(businessId)}/templates`, body);
  }

  updateTemplate(
    businessId: string,
    templateId: string,
    body: Partial<{
      name: string;
      subject: string;
      preheader: string | null;
      body_markdown: string;
      track_opens: boolean;
      track_clicks: boolean;
    }>,
  ): Observable<MailingTemplate> {
    return this.http.patch<MailingTemplate>(
      `${this.base(businessId)}/templates/${templateId}`,
      body,
    );
  }

  deleteTemplate(businessId: string, templateId: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/templates/${templateId}`);
  }
}
