"""Generated immutable resource scopes (OpenAPI Generator operations)."""
from __future__ import annotations
from types import MappingProxyType
from collections.abc import Mapping
from manyforge.transport import SyncTransport, AsyncTransport
from manyforge.api.analytics_analytics_resource import AnalyticsAnalyticsResource, AsyncAnalyticsAnalyticsResource
from manyforge.api.business_accounting_resource import BusinessAccountingResource, AsyncBusinessAccountingResource
from manyforge.api.business_agent_runs_resource import BusinessAgentRunsResource, AsyncBusinessAgentRunsResource
from manyforge.api.business_agents_resource import BusinessAgentsResource, AsyncBusinessAgentsResource
from manyforge.api.business_agents_models_resource import BusinessAgentsModelsResource, AsyncBusinessAgentsModelsResource
from manyforge.api.business_agents_provider_models_resource import BusinessAgentsProviderModelsResource, AsyncBusinessAgentsProviderModelsResource
from manyforge.api.business_agents_tools_resource import BusinessAgentsToolsResource, AsyncBusinessAgentsToolsResource
from manyforge.api.business_ai_credentials_resource import BusinessAiCredentialsResource, AsyncBusinessAiCredentialsResource
from manyforge.api.business_ai_credentials_codex_models_resource import BusinessAiCredentialsCodexModelsResource, AsyncBusinessAiCredentialsCodexModelsResource
from manyforge.api.business_ai_credentials_codex_pkce_resource import BusinessAiCredentialsCodexPkceResource, AsyncBusinessAiCredentialsCodexPkceResource
from manyforge.api.business_analytics_resource import BusinessAnalyticsResource, AsyncBusinessAnalyticsResource
from manyforge.api.business_analytics_property_rules_resource import BusinessAnalyticsPropertyRulesResource, AsyncBusinessAnalyticsPropertyRulesResource
from manyforge.api.business_approvals_resource import BusinessApprovalsResource, AsyncBusinessApprovalsResource
from manyforge.api.business_audit_resource import BusinessAuditResource, AsyncBusinessAuditResource
from manyforge.api.business_automations_resource import BusinessAutomationsResource, AsyncBusinessAutomationsResource
from manyforge.api.business_automations_enrollments_resource import BusinessAutomationsEnrollmentsResource, AsyncBusinessAutomationsEnrollmentsResource
from manyforge.api.business_automations_stats_resource import BusinessAutomationsStatsResource, AsyncBusinessAutomationsStatsResource
from manyforge.api.business_automations_versions_resource import BusinessAutomationsVersionsResource, AsyncBusinessAutomationsVersionsResource
from manyforge.api.business_automations_versions_graph_resource import BusinessAutomationsVersionsGraphResource, AsyncBusinessAutomationsVersionsGraphResource
from manyforge.api.business_code_reviews_resource import BusinessCodeReviewsResource, AsyncBusinessCodeReviewsResource
from manyforge.api.business_companies_resource import BusinessCompaniesResource, AsyncBusinessCompaniesResource
from manyforge.api.business_connectors_resource import BusinessConnectorsResource, AsyncBusinessConnectorsResource
from manyforge.api.business_connectors_credentials_resource import BusinessConnectorsCredentialsResource, AsyncBusinessConnectorsCredentialsResource
from manyforge.api.business_connectors_failed_ops_resource import BusinessConnectorsFailedOpsResource, AsyncBusinessConnectorsFailedOpsResource
from manyforge.api.business_contacts_resource import BusinessContactsResource, AsyncBusinessContactsResource
from manyforge.api.business_contacts_activity_resource import BusinessContactsActivityResource, AsyncBusinessContactsActivityResource
from manyforge.api.business_feedback_boards_resource import BusinessFeedbackBoardsResource, AsyncBusinessFeedbackBoardsResource
from manyforge.api.business_feedback_keys_resource import BusinessFeedbackKeysResource, AsyncBusinessFeedbackKeysResource
from manyforge.api.business_feedback_posts_resource import BusinessFeedbackPostsResource, AsyncBusinessFeedbackPostsResource
from manyforge.api.business_inbox_email_domains_resource import BusinessInboxEmailDomainsResource, AsyncBusinessInboxEmailDomainsResource
from manyforge.api.business_inbox_inbound_addresses_resource import BusinessInboxInboundAddressesResource, AsyncBusinessInboxInboundAddressesResource
from manyforge.api.business_invitations_resource import BusinessInvitationsResource, AsyncBusinessInvitationsResource
from manyforge.api.business_mailing_campaigns_resource import BusinessMailingCampaignsResource, AsyncBusinessMailingCampaignsResource
from manyforge.api.business_mailing_campaigns_stats_resource import BusinessMailingCampaignsStatsResource, AsyncBusinessMailingCampaignsStatsResource
from manyforge.api.business_mailing_deliveries_resource import BusinessMailingDeliveriesResource, AsyncBusinessMailingDeliveriesResource
from manyforge.api.business_mailing_events_resource import BusinessMailingEventsResource, AsyncBusinessMailingEventsResource
from manyforge.api.business_mailing_keys_resource import BusinessMailingKeysResource, AsyncBusinessMailingKeysResource
from manyforge.api.business_mailing_lists_resource import BusinessMailingListsResource, AsyncBusinessMailingListsResource
from manyforge.api.business_mailing_sending_profile_resource import BusinessMailingSendingProfileResource, AsyncBusinessMailingSendingProfileResource
from manyforge.api.business_mailing_setup_resource import BusinessMailingSetupResource, AsyncBusinessMailingSetupResource
from manyforge.api.business_mailing_subscribers_resource import BusinessMailingSubscribersResource, AsyncBusinessMailingSubscribersResource
from manyforge.api.business_mailing_suppressions_resource import BusinessMailingSuppressionsResource, AsyncBusinessMailingSuppressionsResource
from manyforge.api.business_mailing_templates_resource import BusinessMailingTemplatesResource, AsyncBusinessMailingTemplatesResource
from manyforge.api.business_mcp_servers_resource import BusinessMcpServersResource, AsyncBusinessMcpServersResource
from manyforge.api.business_mcp_servers_tool_policies_resource import BusinessMcpServersToolPoliciesResource, AsyncBusinessMcpServersToolPoliciesResource
from manyforge.api.business_mcp_servers_tools_resource import BusinessMcpServersToolsResource, AsyncBusinessMcpServersToolsResource
from manyforge.api.business_members_resource import BusinessMembersResource, AsyncBusinessMembersResource
from manyforge.api.business_repo_connectors_resource import BusinessRepoConnectorsResource, AsyncBusinessRepoConnectorsResource
from manyforge.api.business_repo_connectors_dimension_overrides_resource import BusinessRepoConnectorsDimensionOverridesResource, AsyncBusinessRepoConnectorsDimensionOverridesResource
from manyforge.api.business_requesters_resource import BusinessRequestersResource, AsyncBusinessRequestersResource
from manyforge.api.business_review_config_resource import BusinessReviewConfigResource, AsyncBusinessReviewConfigResource
from manyforge.api.business_review_dimensions_resource import BusinessReviewDimensionsResource, AsyncBusinessReviewDimensionsResource
from manyforge.api.business_roles_resource import BusinessRolesResource, AsyncBusinessRolesResource
from manyforge.api.business_telemetry_clients_resource import BusinessTelemetryClientsResource, AsyncBusinessTelemetryClientsResource
from manyforge.api.business_telemetry_clients_allowed_origins_resource import BusinessTelemetryClientsAllowedOriginsResource, AsyncBusinessTelemetryClientsAllowedOriginsResource
from manyforge.api.business_telemetry_clients_move_targets_resource import BusinessTelemetryClientsMoveTargetsResource, AsyncBusinessTelemetryClientsMoveTargetsResource
from manyforge.api.business_tickets_resource import BusinessTicketsResource, AsyncBusinessTicketsResource
from manyforge.api.business_tickets_assignable_members_resource import BusinessTicketsAssignableMembersResource, AsyncBusinessTicketsAssignableMembersResource
from manyforge.api.business_tickets_messages_resource import BusinessTicketsMessagesResource, AsyncBusinessTicketsMessagesResource
from manyforge.api.feedback_posts_resource import FeedbackPostsResource, AsyncFeedbackPostsResource
from manyforge.api.mailing_mailing_resource import MailingMailingResource, AsyncMailingMailingResource
from manyforge.api.mailing_server_mailing_events_resource import MailingServerMailingEventsResource, AsyncMailingServerMailingEventsResource
from manyforge.api.mailing_server_mailing_subscribers_resource import MailingServerMailingSubscribersResource, AsyncMailingServerMailingSubscribersResource
from manyforge.api.root_account_resource import RootAccountResource, AsyncRootAccountResource
from manyforge.api.root_analytics_resource import RootAnalyticsResource, AsyncRootAnalyticsResource
from manyforge.api.root_auth_resource import RootAuthResource, AsyncRootAuthResource
from manyforge.api.root_businesses_resource import RootBusinessesResource, AsyncRootBusinessesResource
from manyforge.api.root_github_app_install_url_resource import RootGithubAppInstallUrlResource, AsyncRootGithubAppInstallUrlResource
from manyforge.api.root_github_app_installations_resource import RootGithubAppInstallationsResource, AsyncRootGithubAppInstallationsResource
from manyforge.api.root_invitations_resource import RootInvitationsResource, AsyncRootInvitationsResource
from manyforge.api.root_mailing_reporting_resource import RootMailingReportingResource, AsyncRootMailingReportingResource
from manyforge.api.root_permissions_resource import RootPermissionsResource, AsyncRootPermissionsResource
from manyforge.api.root_tenant_merges_resource import RootTenantMergesResource, AsyncRootTenantMergesResource
from manyforge.api.root_tenant_merges_options_resource import RootTenantMergesOptionsResource, AsyncRootTenantMergesOptionsResource
from manyforge.api.telemetry_telemetry_resource import TelemetryTelemetryResource, AsyncTelemetryTelemetryResource

class BusinessAiCredentialsCodexModelsScope(BusinessAiCredentialsCodexModelsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAiCredentialsCodexPkceScope(BusinessAiCredentialsCodexPkceResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAutomationsVersionsGraphScope(BusinessAutomationsVersionsGraphResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMailingCampaignsStatsScope(BusinessMailingCampaignsStatsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessTelemetryClientsAllowedOriginsScope(BusinessTelemetryClientsAllowedOriginsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessTelemetryClientsMoveTargetsScope(BusinessTelemetryClientsMoveTargetsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAgentsModelsScope(BusinessAgentsModelsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAgentsProviderModelsScope(BusinessAgentsProviderModelsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAgentsToolsScope(BusinessAgentsToolsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAiCredentialsCodexScope(object):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def models(self) -> BusinessAiCredentialsCodexModelsScope:
        return BusinessAiCredentialsCodexModelsScope(self._transport, self._bindings)

    @property
    def pkce(self) -> BusinessAiCredentialsCodexPkceScope:
        return BusinessAiCredentialsCodexPkceScope(self._transport, self._bindings)

class BusinessAnalyticsPropertyRulesScope(BusinessAnalyticsPropertyRulesResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAutomationsEnrollmentsScope(BusinessAutomationsEnrollmentsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAutomationsStatsScope(BusinessAutomationsStatsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAutomationsVersionsScope(BusinessAutomationsVersionsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def graph(self) -> BusinessAutomationsVersionsGraphScope:
        return BusinessAutomationsVersionsGraphScope(self._transport, self._bindings)

class BusinessConnectorsCredentialsScope(BusinessConnectorsCredentialsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessConnectorsFailedOpsScope(BusinessConnectorsFailedOpsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessContactsActivityScope(BusinessContactsActivityResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessFeedbackBoardsScope(BusinessFeedbackBoardsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessFeedbackKeysScope(BusinessFeedbackKeysResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessFeedbackPostsScope(BusinessFeedbackPostsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessInboxEmailDomainsScope(BusinessInboxEmailDomainsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessInboxInboundAddressesScope(BusinessInboxInboundAddressesResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMailingCampaignsScope(BusinessMailingCampaignsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def stats(self) -> BusinessMailingCampaignsStatsScope:
        return BusinessMailingCampaignsStatsScope(self._transport, self._bindings)

class BusinessMailingDeliveriesScope(BusinessMailingDeliveriesResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMailingEventsScope(BusinessMailingEventsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMailingKeysScope(BusinessMailingKeysResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMailingListsScope(BusinessMailingListsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMailingSendingProfileScope(BusinessMailingSendingProfileResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMailingSetupScope(BusinessMailingSetupResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMailingSubscribersScope(BusinessMailingSubscribersResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMailingSuppressionsScope(BusinessMailingSuppressionsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMailingTemplatesScope(BusinessMailingTemplatesResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMcpServersToolPoliciesScope(BusinessMcpServersToolPoliciesResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMcpServersToolsScope(BusinessMcpServersToolsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessRepoConnectorsDimensionOverridesScope(BusinessRepoConnectorsDimensionOverridesResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessTelemetryClientsScope(BusinessTelemetryClientsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def allowed_origins(self) -> BusinessTelemetryClientsAllowedOriginsScope:
        return BusinessTelemetryClientsAllowedOriginsScope(self._transport, self._bindings)

    @property
    def move_targets(self) -> BusinessTelemetryClientsMoveTargetsScope:
        return BusinessTelemetryClientsMoveTargetsScope(self._transport, self._bindings)

class BusinessTicketsAssignableMembersScope(BusinessTicketsAssignableMembersResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessTicketsMessagesScope(BusinessTicketsMessagesResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class RootGithubAppInstallUrlScope(RootGithubAppInstallUrlResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class RootGithubAppInstallationsScope(RootGithubAppInstallationsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class RootMailingReportingScope(RootMailingReportingResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class RootTenantMergesOptionsScope(RootTenantMergesOptionsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AnalyticsScope(AnalyticsAnalyticsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessScope(object):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def accounting(self) -> BusinessAccountingScope:
        return BusinessAccountingScope(self._transport, self._bindings)

    @property
    def agent_runs(self) -> BusinessAgentRunsScope:
        return BusinessAgentRunsScope(self._transport, self._bindings)

    @property
    def agents(self) -> BusinessAgentsScope:
        return BusinessAgentsScope(self._transport, self._bindings)

    @property
    def ai_credentials(self) -> BusinessAiCredentialsScope:
        return BusinessAiCredentialsScope(self._transport, self._bindings)

    @property
    def analytics(self) -> BusinessAnalyticsScope:
        return BusinessAnalyticsScope(self._transport, self._bindings)

    @property
    def approvals(self) -> BusinessApprovalsScope:
        return BusinessApprovalsScope(self._transport, self._bindings)

    @property
    def audit(self) -> BusinessAuditScope:
        return BusinessAuditScope(self._transport, self._bindings)

    @property
    def automations(self) -> BusinessAutomationsScope:
        return BusinessAutomationsScope(self._transport, self._bindings)

    @property
    def code_reviews(self) -> BusinessCodeReviewsScope:
        return BusinessCodeReviewsScope(self._transport, self._bindings)

    @property
    def companies(self) -> BusinessCompaniesScope:
        return BusinessCompaniesScope(self._transport, self._bindings)

    @property
    def connectors(self) -> BusinessConnectorsScope:
        return BusinessConnectorsScope(self._transport, self._bindings)

    @property
    def contacts(self) -> BusinessContactsScope:
        return BusinessContactsScope(self._transport, self._bindings)

    @property
    def feedback(self) -> BusinessFeedbackScope:
        return BusinessFeedbackScope(self._transport, self._bindings)

    @property
    def inbox(self) -> BusinessInboxScope:
        return BusinessInboxScope(self._transport, self._bindings)

    @property
    def invitations(self) -> BusinessInvitationsScope:
        return BusinessInvitationsScope(self._transport, self._bindings)

    @property
    def mailing(self) -> BusinessMailingScope:
        return BusinessMailingScope(self._transport, self._bindings)

    @property
    def mcp_servers(self) -> BusinessMcpServersScope:
        return BusinessMcpServersScope(self._transport, self._bindings)

    @property
    def members(self) -> BusinessMembersScope:
        return BusinessMembersScope(self._transport, self._bindings)

    @property
    def repo_connectors(self) -> BusinessRepoConnectorsScope:
        return BusinessRepoConnectorsScope(self._transport, self._bindings)

    @property
    def requesters(self) -> BusinessRequestersScope:
        return BusinessRequestersScope(self._transport, self._bindings)

    @property
    def review_config(self) -> BusinessReviewConfigScope:
        return BusinessReviewConfigScope(self._transport, self._bindings)

    @property
    def review_dimensions(self) -> BusinessReviewDimensionsScope:
        return BusinessReviewDimensionsScope(self._transport, self._bindings)

    @property
    def roles(self) -> BusinessRolesScope:
        return BusinessRolesScope(self._transport, self._bindings)

    @property
    def telemetry(self) -> BusinessTelemetryScope:
        return BusinessTelemetryScope(self._transport, self._bindings)

    @property
    def tickets(self) -> BusinessTicketsScope:
        return BusinessTicketsScope(self._transport, self._bindings)

class BusinessAccountingScope(BusinessAccountingResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAgentRunsScope(BusinessAgentRunsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAgentsScope(BusinessAgentsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def models(self) -> BusinessAgentsModelsScope:
        return BusinessAgentsModelsScope(self._transport, self._bindings)

    @property
    def provider_models(self) -> BusinessAgentsProviderModelsScope:
        return BusinessAgentsProviderModelsScope(self._transport, self._bindings)

    @property
    def tools(self) -> BusinessAgentsToolsScope:
        return BusinessAgentsToolsScope(self._transport, self._bindings)

class BusinessAiCredentialsScope(BusinessAiCredentialsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def codex(self) -> BusinessAiCredentialsCodexScope:
        return BusinessAiCredentialsCodexScope(self._transport, self._bindings)

class BusinessAnalyticsScope(BusinessAnalyticsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def property_rules(self) -> BusinessAnalyticsPropertyRulesScope:
        return BusinessAnalyticsPropertyRulesScope(self._transport, self._bindings)

class BusinessApprovalsScope(BusinessApprovalsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAuditScope(BusinessAuditResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessAutomationsScope(BusinessAutomationsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def enrollments(self) -> BusinessAutomationsEnrollmentsScope:
        return BusinessAutomationsEnrollmentsScope(self._transport, self._bindings)

    @property
    def stats(self) -> BusinessAutomationsStatsScope:
        return BusinessAutomationsStatsScope(self._transport, self._bindings)

    @property
    def versions(self) -> BusinessAutomationsVersionsScope:
        return BusinessAutomationsVersionsScope(self._transport, self._bindings)

class BusinessCodeReviewsScope(BusinessCodeReviewsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessCompaniesScope(BusinessCompaniesResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessConnectorsScope(BusinessConnectorsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def credentials(self) -> BusinessConnectorsCredentialsScope:
        return BusinessConnectorsCredentialsScope(self._transport, self._bindings)

    @property
    def failed_ops(self) -> BusinessConnectorsFailedOpsScope:
        return BusinessConnectorsFailedOpsScope(self._transport, self._bindings)

class BusinessContactsScope(BusinessContactsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def activity(self) -> BusinessContactsActivityScope:
        return BusinessContactsActivityScope(self._transport, self._bindings)

class BusinessFeedbackScope(object):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def boards(self) -> BusinessFeedbackBoardsScope:
        return BusinessFeedbackBoardsScope(self._transport, self._bindings)

    @property
    def keys(self) -> BusinessFeedbackKeysScope:
        return BusinessFeedbackKeysScope(self._transport, self._bindings)

    @property
    def posts(self) -> BusinessFeedbackPostsScope:
        return BusinessFeedbackPostsScope(self._transport, self._bindings)

class BusinessInboxScope(object):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def email_domains(self) -> BusinessInboxEmailDomainsScope:
        return BusinessInboxEmailDomainsScope(self._transport, self._bindings)

    @property
    def inbound_addresses(self) -> BusinessInboxInboundAddressesScope:
        return BusinessInboxInboundAddressesScope(self._transport, self._bindings)

class BusinessInvitationsScope(BusinessInvitationsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessMailingScope(object):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def campaigns(self) -> BusinessMailingCampaignsScope:
        return BusinessMailingCampaignsScope(self._transport, self._bindings)

    @property
    def deliveries(self) -> BusinessMailingDeliveriesScope:
        return BusinessMailingDeliveriesScope(self._transport, self._bindings)

    @property
    def events(self) -> BusinessMailingEventsScope:
        return BusinessMailingEventsScope(self._transport, self._bindings)

    @property
    def keys(self) -> BusinessMailingKeysScope:
        return BusinessMailingKeysScope(self._transport, self._bindings)

    @property
    def lists(self) -> BusinessMailingListsScope:
        return BusinessMailingListsScope(self._transport, self._bindings)

    @property
    def sending_profile(self) -> BusinessMailingSendingProfileScope:
        return BusinessMailingSendingProfileScope(self._transport, self._bindings)

    @property
    def setup(self) -> BusinessMailingSetupScope:
        return BusinessMailingSetupScope(self._transport, self._bindings)

    @property
    def subscribers(self) -> BusinessMailingSubscribersScope:
        return BusinessMailingSubscribersScope(self._transport, self._bindings)

    @property
    def suppressions(self) -> BusinessMailingSuppressionsScope:
        return BusinessMailingSuppressionsScope(self._transport, self._bindings)

    @property
    def templates(self) -> BusinessMailingTemplatesScope:
        return BusinessMailingTemplatesScope(self._transport, self._bindings)

class BusinessMcpServersScope(BusinessMcpServersResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def tool_policies(self) -> BusinessMcpServersToolPoliciesScope:
        return BusinessMcpServersToolPoliciesScope(self._transport, self._bindings)

    @property
    def tools(self) -> BusinessMcpServersToolsScope:
        return BusinessMcpServersToolsScope(self._transport, self._bindings)

class BusinessMembersScope(BusinessMembersResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessRepoConnectorsScope(BusinessRepoConnectorsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def dimension_overrides(self) -> BusinessRepoConnectorsDimensionOverridesScope:
        return BusinessRepoConnectorsDimensionOverridesScope(self._transport, self._bindings)

class BusinessRequestersScope(BusinessRequestersResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessReviewConfigScope(BusinessReviewConfigResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessReviewDimensionsScope(BusinessReviewDimensionsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessRolesScope(BusinessRolesResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class BusinessTelemetryScope(object):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def clients(self) -> BusinessTelemetryClientsScope:
        return BusinessTelemetryClientsScope(self._transport, self._bindings)

class BusinessTicketsScope(BusinessTicketsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def assignable_members(self) -> BusinessTicketsAssignableMembersScope:
        return BusinessTicketsAssignableMembersScope(self._transport, self._bindings)

    @property
    def messages(self) -> BusinessTicketsMessagesScope:
        return BusinessTicketsMessagesScope(self._transport, self._bindings)

class FeedbackScope(object):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def posts(self) -> FeedbackPostsScope:
        return FeedbackPostsScope(self._transport, self._bindings)

class FeedbackPostsScope(FeedbackPostsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class MailingScope(MailingMailingResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class MailingServerScope(object):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def events(self) -> MailingServerEventsScope:
        return MailingServerEventsScope(self._transport, self._bindings)

    @property
    def subscribers(self) -> MailingServerSubscribersScope:
        return MailingServerSubscribersScope(self._transport, self._bindings)

class MailingServerEventsScope(MailingServerMailingEventsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class MailingServerSubscribersScope(MailingServerMailingSubscribersResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class RootScope(object):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def account(self) -> RootAccountScope:
        return RootAccountScope(self._transport, self._bindings)

    @property
    def analytics(self) -> RootAnalyticsScope:
        return RootAnalyticsScope(self._transport, self._bindings)

    @property
    def auth(self) -> RootAuthScope:
        return RootAuthScope(self._transport, self._bindings)

    @property
    def businesses(self) -> RootBusinessesScope:
        return RootBusinessesScope(self._transport, self._bindings)

    @property
    def github_app(self) -> RootGithubAppScope:
        return RootGithubAppScope(self._transport, self._bindings)

    @property
    def invitations(self) -> RootInvitationsScope:
        return RootInvitationsScope(self._transport, self._bindings)

    @property
    def mailing(self) -> RootMailingScope:
        return RootMailingScope(self._transport, self._bindings)

    @property
    def permissions(self) -> RootPermissionsScope:
        return RootPermissionsScope(self._transport, self._bindings)

    @property
    def tenant_merges(self) -> RootTenantMergesScope:
        return RootTenantMergesScope(self._transport, self._bindings)

class RootAccountScope(RootAccountResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class RootAnalyticsScope(RootAnalyticsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class RootAuthScope(RootAuthResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class RootBusinessesScope(RootBusinessesResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class RootGithubAppScope(object):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def install_url(self) -> RootGithubAppInstallUrlScope:
        return RootGithubAppInstallUrlScope(self._transport, self._bindings)

    @property
    def installations(self) -> RootGithubAppInstallationsScope:
        return RootGithubAppInstallationsScope(self._transport, self._bindings)

class RootInvitationsScope(RootInvitationsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class RootMailingScope(object):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def reporting(self) -> RootMailingReportingScope:
        return RootMailingReportingScope(self._transport, self._bindings)

class RootPermissionsScope(RootPermissionsResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class RootTenantMergesScope(RootTenantMergesResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def options(self) -> RootTenantMergesOptionsScope:
        return RootTenantMergesOptionsScope(self._transport, self._bindings)

class TelemetryScope(TelemetryTelemetryResource):
    def __init__(self, transport: SyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAiCredentialsCodexModelsScope(AsyncBusinessAiCredentialsCodexModelsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAiCredentialsCodexPkceScope(AsyncBusinessAiCredentialsCodexPkceResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAutomationsVersionsGraphScope(AsyncBusinessAutomationsVersionsGraphResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMailingCampaignsStatsScope(AsyncBusinessMailingCampaignsStatsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessTelemetryClientsAllowedOriginsScope(AsyncBusinessTelemetryClientsAllowedOriginsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessTelemetryClientsMoveTargetsScope(AsyncBusinessTelemetryClientsMoveTargetsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAgentsModelsScope(AsyncBusinessAgentsModelsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAgentsProviderModelsScope(AsyncBusinessAgentsProviderModelsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAgentsToolsScope(AsyncBusinessAgentsToolsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAiCredentialsCodexScope(object):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def models(self) -> AsyncBusinessAiCredentialsCodexModelsScope:
        return AsyncBusinessAiCredentialsCodexModelsScope(self._transport, self._bindings)

    @property
    def pkce(self) -> AsyncBusinessAiCredentialsCodexPkceScope:
        return AsyncBusinessAiCredentialsCodexPkceScope(self._transport, self._bindings)

class AsyncBusinessAnalyticsPropertyRulesScope(AsyncBusinessAnalyticsPropertyRulesResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAutomationsEnrollmentsScope(AsyncBusinessAutomationsEnrollmentsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAutomationsStatsScope(AsyncBusinessAutomationsStatsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAutomationsVersionsScope(AsyncBusinessAutomationsVersionsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def graph(self) -> AsyncBusinessAutomationsVersionsGraphScope:
        return AsyncBusinessAutomationsVersionsGraphScope(self._transport, self._bindings)

class AsyncBusinessConnectorsCredentialsScope(AsyncBusinessConnectorsCredentialsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessConnectorsFailedOpsScope(AsyncBusinessConnectorsFailedOpsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessContactsActivityScope(AsyncBusinessContactsActivityResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessFeedbackBoardsScope(AsyncBusinessFeedbackBoardsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessFeedbackKeysScope(AsyncBusinessFeedbackKeysResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessFeedbackPostsScope(AsyncBusinessFeedbackPostsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessInboxEmailDomainsScope(AsyncBusinessInboxEmailDomainsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessInboxInboundAddressesScope(AsyncBusinessInboxInboundAddressesResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMailingCampaignsScope(AsyncBusinessMailingCampaignsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def stats(self) -> AsyncBusinessMailingCampaignsStatsScope:
        return AsyncBusinessMailingCampaignsStatsScope(self._transport, self._bindings)

class AsyncBusinessMailingDeliveriesScope(AsyncBusinessMailingDeliveriesResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMailingEventsScope(AsyncBusinessMailingEventsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMailingKeysScope(AsyncBusinessMailingKeysResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMailingListsScope(AsyncBusinessMailingListsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMailingSendingProfileScope(AsyncBusinessMailingSendingProfileResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMailingSetupScope(AsyncBusinessMailingSetupResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMailingSubscribersScope(AsyncBusinessMailingSubscribersResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMailingSuppressionsScope(AsyncBusinessMailingSuppressionsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMailingTemplatesScope(AsyncBusinessMailingTemplatesResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMcpServersToolPoliciesScope(AsyncBusinessMcpServersToolPoliciesResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMcpServersToolsScope(AsyncBusinessMcpServersToolsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessRepoConnectorsDimensionOverridesScope(AsyncBusinessRepoConnectorsDimensionOverridesResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessTelemetryClientsScope(AsyncBusinessTelemetryClientsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def allowed_origins(self) -> AsyncBusinessTelemetryClientsAllowedOriginsScope:
        return AsyncBusinessTelemetryClientsAllowedOriginsScope(self._transport, self._bindings)

    @property
    def move_targets(self) -> AsyncBusinessTelemetryClientsMoveTargetsScope:
        return AsyncBusinessTelemetryClientsMoveTargetsScope(self._transport, self._bindings)

class AsyncBusinessTicketsAssignableMembersScope(AsyncBusinessTicketsAssignableMembersResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessTicketsMessagesScope(AsyncBusinessTicketsMessagesResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncRootGithubAppInstallUrlScope(AsyncRootGithubAppInstallUrlResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncRootGithubAppInstallationsScope(AsyncRootGithubAppInstallationsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncRootMailingReportingScope(AsyncRootMailingReportingResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncRootTenantMergesOptionsScope(AsyncRootTenantMergesOptionsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncAnalyticsScope(AsyncAnalyticsAnalyticsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessScope(object):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def accounting(self) -> AsyncBusinessAccountingScope:
        return AsyncBusinessAccountingScope(self._transport, self._bindings)

    @property
    def agent_runs(self) -> AsyncBusinessAgentRunsScope:
        return AsyncBusinessAgentRunsScope(self._transport, self._bindings)

    @property
    def agents(self) -> AsyncBusinessAgentsScope:
        return AsyncBusinessAgentsScope(self._transport, self._bindings)

    @property
    def ai_credentials(self) -> AsyncBusinessAiCredentialsScope:
        return AsyncBusinessAiCredentialsScope(self._transport, self._bindings)

    @property
    def analytics(self) -> AsyncBusinessAnalyticsScope:
        return AsyncBusinessAnalyticsScope(self._transport, self._bindings)

    @property
    def approvals(self) -> AsyncBusinessApprovalsScope:
        return AsyncBusinessApprovalsScope(self._transport, self._bindings)

    @property
    def audit(self) -> AsyncBusinessAuditScope:
        return AsyncBusinessAuditScope(self._transport, self._bindings)

    @property
    def automations(self) -> AsyncBusinessAutomationsScope:
        return AsyncBusinessAutomationsScope(self._transport, self._bindings)

    @property
    def code_reviews(self) -> AsyncBusinessCodeReviewsScope:
        return AsyncBusinessCodeReviewsScope(self._transport, self._bindings)

    @property
    def companies(self) -> AsyncBusinessCompaniesScope:
        return AsyncBusinessCompaniesScope(self._transport, self._bindings)

    @property
    def connectors(self) -> AsyncBusinessConnectorsScope:
        return AsyncBusinessConnectorsScope(self._transport, self._bindings)

    @property
    def contacts(self) -> AsyncBusinessContactsScope:
        return AsyncBusinessContactsScope(self._transport, self._bindings)

    @property
    def feedback(self) -> AsyncBusinessFeedbackScope:
        return AsyncBusinessFeedbackScope(self._transport, self._bindings)

    @property
    def inbox(self) -> AsyncBusinessInboxScope:
        return AsyncBusinessInboxScope(self._transport, self._bindings)

    @property
    def invitations(self) -> AsyncBusinessInvitationsScope:
        return AsyncBusinessInvitationsScope(self._transport, self._bindings)

    @property
    def mailing(self) -> AsyncBusinessMailingScope:
        return AsyncBusinessMailingScope(self._transport, self._bindings)

    @property
    def mcp_servers(self) -> AsyncBusinessMcpServersScope:
        return AsyncBusinessMcpServersScope(self._transport, self._bindings)

    @property
    def members(self) -> AsyncBusinessMembersScope:
        return AsyncBusinessMembersScope(self._transport, self._bindings)

    @property
    def repo_connectors(self) -> AsyncBusinessRepoConnectorsScope:
        return AsyncBusinessRepoConnectorsScope(self._transport, self._bindings)

    @property
    def requesters(self) -> AsyncBusinessRequestersScope:
        return AsyncBusinessRequestersScope(self._transport, self._bindings)

    @property
    def review_config(self) -> AsyncBusinessReviewConfigScope:
        return AsyncBusinessReviewConfigScope(self._transport, self._bindings)

    @property
    def review_dimensions(self) -> AsyncBusinessReviewDimensionsScope:
        return AsyncBusinessReviewDimensionsScope(self._transport, self._bindings)

    @property
    def roles(self) -> AsyncBusinessRolesScope:
        return AsyncBusinessRolesScope(self._transport, self._bindings)

    @property
    def telemetry(self) -> AsyncBusinessTelemetryScope:
        return AsyncBusinessTelemetryScope(self._transport, self._bindings)

    @property
    def tickets(self) -> AsyncBusinessTicketsScope:
        return AsyncBusinessTicketsScope(self._transport, self._bindings)

class AsyncBusinessAccountingScope(AsyncBusinessAccountingResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAgentRunsScope(AsyncBusinessAgentRunsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAgentsScope(AsyncBusinessAgentsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def models(self) -> AsyncBusinessAgentsModelsScope:
        return AsyncBusinessAgentsModelsScope(self._transport, self._bindings)

    @property
    def provider_models(self) -> AsyncBusinessAgentsProviderModelsScope:
        return AsyncBusinessAgentsProviderModelsScope(self._transport, self._bindings)

    @property
    def tools(self) -> AsyncBusinessAgentsToolsScope:
        return AsyncBusinessAgentsToolsScope(self._transport, self._bindings)

class AsyncBusinessAiCredentialsScope(AsyncBusinessAiCredentialsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def codex(self) -> AsyncBusinessAiCredentialsCodexScope:
        return AsyncBusinessAiCredentialsCodexScope(self._transport, self._bindings)

class AsyncBusinessAnalyticsScope(AsyncBusinessAnalyticsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def property_rules(self) -> AsyncBusinessAnalyticsPropertyRulesScope:
        return AsyncBusinessAnalyticsPropertyRulesScope(self._transport, self._bindings)

class AsyncBusinessApprovalsScope(AsyncBusinessApprovalsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAuditScope(AsyncBusinessAuditResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessAutomationsScope(AsyncBusinessAutomationsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def enrollments(self) -> AsyncBusinessAutomationsEnrollmentsScope:
        return AsyncBusinessAutomationsEnrollmentsScope(self._transport, self._bindings)

    @property
    def stats(self) -> AsyncBusinessAutomationsStatsScope:
        return AsyncBusinessAutomationsStatsScope(self._transport, self._bindings)

    @property
    def versions(self) -> AsyncBusinessAutomationsVersionsScope:
        return AsyncBusinessAutomationsVersionsScope(self._transport, self._bindings)

class AsyncBusinessCodeReviewsScope(AsyncBusinessCodeReviewsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessCompaniesScope(AsyncBusinessCompaniesResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessConnectorsScope(AsyncBusinessConnectorsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def credentials(self) -> AsyncBusinessConnectorsCredentialsScope:
        return AsyncBusinessConnectorsCredentialsScope(self._transport, self._bindings)

    @property
    def failed_ops(self) -> AsyncBusinessConnectorsFailedOpsScope:
        return AsyncBusinessConnectorsFailedOpsScope(self._transport, self._bindings)

class AsyncBusinessContactsScope(AsyncBusinessContactsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def activity(self) -> AsyncBusinessContactsActivityScope:
        return AsyncBusinessContactsActivityScope(self._transport, self._bindings)

class AsyncBusinessFeedbackScope(object):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def boards(self) -> AsyncBusinessFeedbackBoardsScope:
        return AsyncBusinessFeedbackBoardsScope(self._transport, self._bindings)

    @property
    def keys(self) -> AsyncBusinessFeedbackKeysScope:
        return AsyncBusinessFeedbackKeysScope(self._transport, self._bindings)

    @property
    def posts(self) -> AsyncBusinessFeedbackPostsScope:
        return AsyncBusinessFeedbackPostsScope(self._transport, self._bindings)

class AsyncBusinessInboxScope(object):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def email_domains(self) -> AsyncBusinessInboxEmailDomainsScope:
        return AsyncBusinessInboxEmailDomainsScope(self._transport, self._bindings)

    @property
    def inbound_addresses(self) -> AsyncBusinessInboxInboundAddressesScope:
        return AsyncBusinessInboxInboundAddressesScope(self._transport, self._bindings)

class AsyncBusinessInvitationsScope(AsyncBusinessInvitationsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessMailingScope(object):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def campaigns(self) -> AsyncBusinessMailingCampaignsScope:
        return AsyncBusinessMailingCampaignsScope(self._transport, self._bindings)

    @property
    def deliveries(self) -> AsyncBusinessMailingDeliveriesScope:
        return AsyncBusinessMailingDeliveriesScope(self._transport, self._bindings)

    @property
    def events(self) -> AsyncBusinessMailingEventsScope:
        return AsyncBusinessMailingEventsScope(self._transport, self._bindings)

    @property
    def keys(self) -> AsyncBusinessMailingKeysScope:
        return AsyncBusinessMailingKeysScope(self._transport, self._bindings)

    @property
    def lists(self) -> AsyncBusinessMailingListsScope:
        return AsyncBusinessMailingListsScope(self._transport, self._bindings)

    @property
    def sending_profile(self) -> AsyncBusinessMailingSendingProfileScope:
        return AsyncBusinessMailingSendingProfileScope(self._transport, self._bindings)

    @property
    def setup(self) -> AsyncBusinessMailingSetupScope:
        return AsyncBusinessMailingSetupScope(self._transport, self._bindings)

    @property
    def subscribers(self) -> AsyncBusinessMailingSubscribersScope:
        return AsyncBusinessMailingSubscribersScope(self._transport, self._bindings)

    @property
    def suppressions(self) -> AsyncBusinessMailingSuppressionsScope:
        return AsyncBusinessMailingSuppressionsScope(self._transport, self._bindings)

    @property
    def templates(self) -> AsyncBusinessMailingTemplatesScope:
        return AsyncBusinessMailingTemplatesScope(self._transport, self._bindings)

class AsyncBusinessMcpServersScope(AsyncBusinessMcpServersResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def tool_policies(self) -> AsyncBusinessMcpServersToolPoliciesScope:
        return AsyncBusinessMcpServersToolPoliciesScope(self._transport, self._bindings)

    @property
    def tools(self) -> AsyncBusinessMcpServersToolsScope:
        return AsyncBusinessMcpServersToolsScope(self._transport, self._bindings)

class AsyncBusinessMembersScope(AsyncBusinessMembersResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessRepoConnectorsScope(AsyncBusinessRepoConnectorsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def dimension_overrides(self) -> AsyncBusinessRepoConnectorsDimensionOverridesScope:
        return AsyncBusinessRepoConnectorsDimensionOverridesScope(self._transport, self._bindings)

class AsyncBusinessRequestersScope(AsyncBusinessRequestersResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessReviewConfigScope(AsyncBusinessReviewConfigResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessReviewDimensionsScope(AsyncBusinessReviewDimensionsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessRolesScope(AsyncBusinessRolesResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncBusinessTelemetryScope(object):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def clients(self) -> AsyncBusinessTelemetryClientsScope:
        return AsyncBusinessTelemetryClientsScope(self._transport, self._bindings)

class AsyncBusinessTicketsScope(AsyncBusinessTicketsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def assignable_members(self) -> AsyncBusinessTicketsAssignableMembersScope:
        return AsyncBusinessTicketsAssignableMembersScope(self._transport, self._bindings)

    @property
    def messages(self) -> AsyncBusinessTicketsMessagesScope:
        return AsyncBusinessTicketsMessagesScope(self._transport, self._bindings)

class AsyncFeedbackScope(object):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def posts(self) -> AsyncFeedbackPostsScope:
        return AsyncFeedbackPostsScope(self._transport, self._bindings)

class AsyncFeedbackPostsScope(AsyncFeedbackPostsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncMailingScope(AsyncMailingMailingResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncMailingServerScope(object):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def events(self) -> AsyncMailingServerEventsScope:
        return AsyncMailingServerEventsScope(self._transport, self._bindings)

    @property
    def subscribers(self) -> AsyncMailingServerSubscribersScope:
        return AsyncMailingServerSubscribersScope(self._transport, self._bindings)

class AsyncMailingServerEventsScope(AsyncMailingServerMailingEventsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncMailingServerSubscribersScope(AsyncMailingServerMailingSubscribersResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncRootScope(object):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def account(self) -> AsyncRootAccountScope:
        return AsyncRootAccountScope(self._transport, self._bindings)

    @property
    def analytics(self) -> AsyncRootAnalyticsScope:
        return AsyncRootAnalyticsScope(self._transport, self._bindings)

    @property
    def auth(self) -> AsyncRootAuthScope:
        return AsyncRootAuthScope(self._transport, self._bindings)

    @property
    def businesses(self) -> AsyncRootBusinessesScope:
        return AsyncRootBusinessesScope(self._transport, self._bindings)

    @property
    def github_app(self) -> AsyncRootGithubAppScope:
        return AsyncRootGithubAppScope(self._transport, self._bindings)

    @property
    def invitations(self) -> AsyncRootInvitationsScope:
        return AsyncRootInvitationsScope(self._transport, self._bindings)

    @property
    def mailing(self) -> AsyncRootMailingScope:
        return AsyncRootMailingScope(self._transport, self._bindings)

    @property
    def permissions(self) -> AsyncRootPermissionsScope:
        return AsyncRootPermissionsScope(self._transport, self._bindings)

    @property
    def tenant_merges(self) -> AsyncRootTenantMergesScope:
        return AsyncRootTenantMergesScope(self._transport, self._bindings)

class AsyncRootAccountScope(AsyncRootAccountResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncRootAnalyticsScope(AsyncRootAnalyticsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncRootAuthScope(AsyncRootAuthResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncRootBusinessesScope(AsyncRootBusinessesResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncRootGithubAppScope(object):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def install_url(self) -> AsyncRootGithubAppInstallUrlScope:
        return AsyncRootGithubAppInstallUrlScope(self._transport, self._bindings)

    @property
    def installations(self) -> AsyncRootGithubAppInstallationsScope:
        return AsyncRootGithubAppInstallationsScope(self._transport, self._bindings)

class AsyncRootInvitationsScope(AsyncRootInvitationsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncRootMailingScope(object):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def reporting(self) -> AsyncRootMailingReportingScope:
        return AsyncRootMailingReportingScope(self._transport, self._bindings)

class AsyncRootPermissionsScope(AsyncRootPermissionsResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

class AsyncRootTenantMergesScope(AsyncRootTenantMergesResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))

    @property
    def options(self) -> AsyncRootTenantMergesOptionsScope:
        return AsyncRootTenantMergesOptionsScope(self._transport, self._bindings)

class AsyncTelemetryScope(AsyncTelemetryTelemetryResource):
    def __init__(self, transport: AsyncTransport, bindings: Mapping[str, str] | None = None) -> None:
        self._transport = transport
        self._bindings = MappingProxyType(dict(bindings or {}))
