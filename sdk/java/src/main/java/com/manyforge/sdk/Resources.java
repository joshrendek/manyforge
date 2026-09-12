// Generated resource ownership tree. Do not edit.
package com.manyforge.sdk;

import com.manyforge.sdk.resources.*;
import java.util.Objects;

public final class Resources {
  private Resources() {}
  public static class Analytics extends AnalyticsAnalyticsResource {
    public Analytics(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class Business {
    protected final Transport transport;
    protected final String binding;
    private final BusinessAccounting accounting;
    private final BusinessAgentRuns agentRuns;
    private final BusinessAgents agents;
    private final BusinessAiCredentials aiCredentials;
    private final BusinessAnalytics analytics;
    private final BusinessApprovals approvals;
    private final BusinessAudit audit;
    private final BusinessAutomations automations;
    private final BusinessCodeReviews codeReviews;
    private final BusinessCompanies companies;
    private final BusinessConnectors connectors;
    private final BusinessContacts contacts;
    private final BusinessFeedback feedback;
    private final BusinessInbox inbox;
    private final BusinessInvitations invitations;
    private final BusinessMailing mailing;
    private final BusinessMcpServers mcpServers;
    private final BusinessMembers members;
    private final BusinessRepoConnectors repoConnectors;
    private final BusinessRequesters requesters;
    private final BusinessReviewConfig reviewConfig;
    private final BusinessReviewDimensions reviewDimensions;
    private final BusinessRoles roles;
    private final BusinessTelemetry telemetry;
    private final BusinessTickets tickets;
    public Business(Transport transport, String binding) {
      this.transport = Objects.requireNonNull(transport); this.binding = binding;
      this.accounting = new BusinessAccounting(transport, binding);
      this.agentRuns = new BusinessAgentRuns(transport, binding);
      this.agents = new BusinessAgents(transport, binding);
      this.aiCredentials = new BusinessAiCredentials(transport, binding);
      this.analytics = new BusinessAnalytics(transport, binding);
      this.approvals = new BusinessApprovals(transport, binding);
      this.audit = new BusinessAudit(transport, binding);
      this.automations = new BusinessAutomations(transport, binding);
      this.codeReviews = new BusinessCodeReviews(transport, binding);
      this.companies = new BusinessCompanies(transport, binding);
      this.connectors = new BusinessConnectors(transport, binding);
      this.contacts = new BusinessContacts(transport, binding);
      this.feedback = new BusinessFeedback(transport, binding);
      this.inbox = new BusinessInbox(transport, binding);
      this.invitations = new BusinessInvitations(transport, binding);
      this.mailing = new BusinessMailing(transport, binding);
      this.mcpServers = new BusinessMcpServers(transport, binding);
      this.members = new BusinessMembers(transport, binding);
      this.repoConnectors = new BusinessRepoConnectors(transport, binding);
      this.requesters = new BusinessRequesters(transport, binding);
      this.reviewConfig = new BusinessReviewConfig(transport, binding);
      this.reviewDimensions = new BusinessReviewDimensions(transport, binding);
      this.roles = new BusinessRoles(transport, binding);
      this.telemetry = new BusinessTelemetry(transport, binding);
      this.tickets = new BusinessTickets(transport, binding);
    }
    public BusinessAccounting accounting() { return accounting; }
    public BusinessAgentRuns agentRuns() { return agentRuns; }
    public BusinessAgents agents() { return agents; }
    public BusinessAiCredentials aiCredentials() { return aiCredentials; }
    public BusinessAnalytics analytics() { return analytics; }
    public BusinessApprovals approvals() { return approvals; }
    public BusinessAudit audit() { return audit; }
    public BusinessAutomations automations() { return automations; }
    public BusinessCodeReviews codeReviews() { return codeReviews; }
    public BusinessCompanies companies() { return companies; }
    public BusinessConnectors connectors() { return connectors; }
    public BusinessContacts contacts() { return contacts; }
    public BusinessFeedback feedback() { return feedback; }
    public BusinessInbox inbox() { return inbox; }
    public BusinessInvitations invitations() { return invitations; }
    public BusinessMailing mailing() { return mailing; }
    public BusinessMcpServers mcpServers() { return mcpServers; }
    public BusinessMembers members() { return members; }
    public BusinessRepoConnectors repoConnectors() { return repoConnectors; }
    public BusinessRequesters requesters() { return requesters; }
    public BusinessReviewConfig reviewConfig() { return reviewConfig; }
    public BusinessReviewDimensions reviewDimensions() { return reviewDimensions; }
    public BusinessRoles roles() { return roles; }
    public BusinessTelemetry telemetry() { return telemetry; }
    public BusinessTickets tickets() { return tickets; }
  }
  public static class BusinessAccounting extends BusinessAccountingResource {
    public BusinessAccounting(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessAgentRuns extends BusinessAgentRunsResource {
    public BusinessAgentRuns(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessAgents extends BusinessAgentsResource {
    private final BusinessAgentsModels models;
    private final BusinessAgentsProviderModels providerModels;
    private final BusinessAgentsTools tools;
    public BusinessAgents(Transport transport, String binding) {
      super(transport, binding);
      this.models = new BusinessAgentsModels(transport, binding);
      this.providerModels = new BusinessAgentsProviderModels(transport, binding);
      this.tools = new BusinessAgentsTools(transport, binding);
    }
    public BusinessAgentsModels models() { return models; }
    public BusinessAgentsProviderModels providerModels() { return providerModels; }
    public BusinessAgentsTools tools() { return tools; }
  }
  public static class BusinessAgentsModels extends BusinessAgentsModelsResource {
    public BusinessAgentsModels(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessAgentsProviderModels extends BusinessAgentsProviderModelsResource {
    public BusinessAgentsProviderModels(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessAgentsTools extends BusinessAgentsToolsResource {
    public BusinessAgentsTools(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessAiCredentials extends BusinessAiCredentialsResource {
    private final BusinessAiCredentialsCodex codex;
    public BusinessAiCredentials(Transport transport, String binding) {
      super(transport, binding);
      this.codex = new BusinessAiCredentialsCodex(transport, binding);
    }
    public BusinessAiCredentialsCodex codex() { return codex; }
  }
  public static class BusinessAiCredentialsCodex {
    protected final Transport transport;
    protected final String binding;
    private final BusinessAiCredentialsCodexModels models;
    private final BusinessAiCredentialsCodexPkce pkce;
    public BusinessAiCredentialsCodex(Transport transport, String binding) {
      this.transport = Objects.requireNonNull(transport); this.binding = binding;
      this.models = new BusinessAiCredentialsCodexModels(transport, binding);
      this.pkce = new BusinessAiCredentialsCodexPkce(transport, binding);
    }
    public BusinessAiCredentialsCodexModels models() { return models; }
    public BusinessAiCredentialsCodexPkce pkce() { return pkce; }
  }
  public static class BusinessAiCredentialsCodexModels extends BusinessAiCredentialsCodexModelsResource {
    public BusinessAiCredentialsCodexModels(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessAiCredentialsCodexPkce extends BusinessAiCredentialsCodexPkceResource {
    public BusinessAiCredentialsCodexPkce(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessAnalytics extends BusinessAnalyticsResource {
    private final BusinessAnalyticsPropertyRules propertyRules;
    public BusinessAnalytics(Transport transport, String binding) {
      super(transport, binding);
      this.propertyRules = new BusinessAnalyticsPropertyRules(transport, binding);
    }
    public BusinessAnalyticsPropertyRules propertyRules() { return propertyRules; }
  }
  public static class BusinessAnalyticsPropertyRules extends BusinessAnalyticsPropertyRulesResource {
    public BusinessAnalyticsPropertyRules(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessApprovals extends BusinessApprovalsResource {
    public BusinessApprovals(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessAudit extends BusinessAuditResource {
    public BusinessAudit(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessAutomations extends BusinessAutomationsResource {
    private final BusinessAutomationsEnrollments enrollments;
    private final BusinessAutomationsStats stats;
    private final BusinessAutomationsVersions versions;
    public BusinessAutomations(Transport transport, String binding) {
      super(transport, binding);
      this.enrollments = new BusinessAutomationsEnrollments(transport, binding);
      this.stats = new BusinessAutomationsStats(transport, binding);
      this.versions = new BusinessAutomationsVersions(transport, binding);
    }
    public BusinessAutomationsEnrollments enrollments() { return enrollments; }
    public BusinessAutomationsStats stats() { return stats; }
    public BusinessAutomationsVersions versions() { return versions; }
  }
  public static class BusinessAutomationsEnrollments extends BusinessAutomationsEnrollmentsResource {
    public BusinessAutomationsEnrollments(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessAutomationsStats extends BusinessAutomationsStatsResource {
    public BusinessAutomationsStats(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessAutomationsVersions extends BusinessAutomationsVersionsResource {
    private final BusinessAutomationsVersionsGraph graph;
    public BusinessAutomationsVersions(Transport transport, String binding) {
      super(transport, binding);
      this.graph = new BusinessAutomationsVersionsGraph(transport, binding);
    }
    public BusinessAutomationsVersionsGraph graph() { return graph; }
  }
  public static class BusinessAutomationsVersionsGraph extends BusinessAutomationsVersionsGraphResource {
    public BusinessAutomationsVersionsGraph(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessCodeReviews extends BusinessCodeReviewsResource {
    public BusinessCodeReviews(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessCompanies extends BusinessCompaniesResource {
    public BusinessCompanies(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessConnectors extends BusinessConnectorsResource {
    private final BusinessConnectorsCredentials credentials;
    private final BusinessConnectorsFailedOps failedOps;
    public BusinessConnectors(Transport transport, String binding) {
      super(transport, binding);
      this.credentials = new BusinessConnectorsCredentials(transport, binding);
      this.failedOps = new BusinessConnectorsFailedOps(transport, binding);
    }
    public BusinessConnectorsCredentials credentials() { return credentials; }
    public BusinessConnectorsFailedOps failedOps() { return failedOps; }
  }
  public static class BusinessConnectorsCredentials extends BusinessConnectorsCredentialsResource {
    public BusinessConnectorsCredentials(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessConnectorsFailedOps extends BusinessConnectorsFailedOpsResource {
    public BusinessConnectorsFailedOps(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessContacts extends BusinessContactsResource {
    private final BusinessContactsActivity activity;
    public BusinessContacts(Transport transport, String binding) {
      super(transport, binding);
      this.activity = new BusinessContactsActivity(transport, binding);
    }
    public BusinessContactsActivity activity() { return activity; }
  }
  public static class BusinessContactsActivity extends BusinessContactsActivityResource {
    public BusinessContactsActivity(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessFeedback {
    protected final Transport transport;
    protected final String binding;
    private final BusinessFeedbackBoards boards;
    private final BusinessFeedbackKeys keys;
    private final BusinessFeedbackPosts posts;
    public BusinessFeedback(Transport transport, String binding) {
      this.transport = Objects.requireNonNull(transport); this.binding = binding;
      this.boards = new BusinessFeedbackBoards(transport, binding);
      this.keys = new BusinessFeedbackKeys(transport, binding);
      this.posts = new BusinessFeedbackPosts(transport, binding);
    }
    public BusinessFeedbackBoards boards() { return boards; }
    public BusinessFeedbackKeys keys() { return keys; }
    public BusinessFeedbackPosts posts() { return posts; }
  }
  public static class BusinessFeedbackBoards extends BusinessFeedbackBoardsResource {
    public BusinessFeedbackBoards(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessFeedbackKeys extends BusinessFeedbackKeysResource {
    public BusinessFeedbackKeys(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessFeedbackPosts extends BusinessFeedbackPostsResource {
    public BusinessFeedbackPosts(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessInbox {
    protected final Transport transport;
    protected final String binding;
    private final BusinessInboxEmailDomains emailDomains;
    private final BusinessInboxInboundAddresses inboundAddresses;
    public BusinessInbox(Transport transport, String binding) {
      this.transport = Objects.requireNonNull(transport); this.binding = binding;
      this.emailDomains = new BusinessInboxEmailDomains(transport, binding);
      this.inboundAddresses = new BusinessInboxInboundAddresses(transport, binding);
    }
    public BusinessInboxEmailDomains emailDomains() { return emailDomains; }
    public BusinessInboxInboundAddresses inboundAddresses() { return inboundAddresses; }
  }
  public static class BusinessInboxEmailDomains extends BusinessInboxEmailDomainsResource {
    public BusinessInboxEmailDomains(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessInboxInboundAddresses extends BusinessInboxInboundAddressesResource {
    public BusinessInboxInboundAddresses(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessInvitations extends BusinessInvitationsResource {
    public BusinessInvitations(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMailing {
    protected final Transport transport;
    protected final String binding;
    private final BusinessMailingCampaigns campaigns;
    private final BusinessMailingDeliveries deliveries;
    private final BusinessMailingEvents events;
    private final BusinessMailingKeys keys;
    private final BusinessMailingLists lists;
    private final BusinessMailingSendingProfile sendingProfile;
    private final BusinessMailingSetup setup;
    private final BusinessMailingSubscribers subscribers;
    private final BusinessMailingSuppressions suppressions;
    private final BusinessMailingTemplates templates;
    public BusinessMailing(Transport transport, String binding) {
      this.transport = Objects.requireNonNull(transport); this.binding = binding;
      this.campaigns = new BusinessMailingCampaigns(transport, binding);
      this.deliveries = new BusinessMailingDeliveries(transport, binding);
      this.events = new BusinessMailingEvents(transport, binding);
      this.keys = new BusinessMailingKeys(transport, binding);
      this.lists = new BusinessMailingLists(transport, binding);
      this.sendingProfile = new BusinessMailingSendingProfile(transport, binding);
      this.setup = new BusinessMailingSetup(transport, binding);
      this.subscribers = new BusinessMailingSubscribers(transport, binding);
      this.suppressions = new BusinessMailingSuppressions(transport, binding);
      this.templates = new BusinessMailingTemplates(transport, binding);
    }
    public BusinessMailingCampaigns campaigns() { return campaigns; }
    public BusinessMailingDeliveries deliveries() { return deliveries; }
    public BusinessMailingEvents events() { return events; }
    public BusinessMailingKeys keys() { return keys; }
    public BusinessMailingLists lists() { return lists; }
    public BusinessMailingSendingProfile sendingProfile() { return sendingProfile; }
    public BusinessMailingSetup setup() { return setup; }
    public BusinessMailingSubscribers subscribers() { return subscribers; }
    public BusinessMailingSuppressions suppressions() { return suppressions; }
    public BusinessMailingTemplates templates() { return templates; }
  }
  public static class BusinessMailingCampaigns extends BusinessMailingCampaignsResource {
    private final BusinessMailingCampaignsStats stats;
    public BusinessMailingCampaigns(Transport transport, String binding) {
      super(transport, binding);
      this.stats = new BusinessMailingCampaignsStats(transport, binding);
    }
    public BusinessMailingCampaignsStats stats() { return stats; }
  }
  public static class BusinessMailingCampaignsStats extends BusinessMailingCampaignsStatsResource {
    public BusinessMailingCampaignsStats(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMailingDeliveries extends BusinessMailingDeliveriesResource {
    public BusinessMailingDeliveries(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMailingEvents extends BusinessMailingEventsResource {
    public BusinessMailingEvents(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMailingKeys extends BusinessMailingKeysResource {
    public BusinessMailingKeys(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMailingLists extends BusinessMailingListsResource {
    public BusinessMailingLists(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMailingSendingProfile extends BusinessMailingSendingProfileResource {
    public BusinessMailingSendingProfile(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMailingSetup extends BusinessMailingSetupResource {
    public BusinessMailingSetup(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMailingSubscribers extends BusinessMailingSubscribersResource {
    public BusinessMailingSubscribers(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMailingSuppressions extends BusinessMailingSuppressionsResource {
    public BusinessMailingSuppressions(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMailingTemplates extends BusinessMailingTemplatesResource {
    public BusinessMailingTemplates(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMcpServers extends BusinessMcpServersResource {
    private final BusinessMcpServersToolPolicies toolPolicies;
    private final BusinessMcpServersTools tools;
    public BusinessMcpServers(Transport transport, String binding) {
      super(transport, binding);
      this.toolPolicies = new BusinessMcpServersToolPolicies(transport, binding);
      this.tools = new BusinessMcpServersTools(transport, binding);
    }
    public BusinessMcpServersToolPolicies toolPolicies() { return toolPolicies; }
    public BusinessMcpServersTools tools() { return tools; }
  }
  public static class BusinessMcpServersToolPolicies extends BusinessMcpServersToolPoliciesResource {
    public BusinessMcpServersToolPolicies(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMcpServersTools extends BusinessMcpServersToolsResource {
    public BusinessMcpServersTools(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessMembers extends BusinessMembersResource {
    public BusinessMembers(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessRepoConnectors extends BusinessRepoConnectorsResource {
    private final BusinessRepoConnectorsDimensionOverrides dimensionOverrides;
    public BusinessRepoConnectors(Transport transport, String binding) {
      super(transport, binding);
      this.dimensionOverrides = new BusinessRepoConnectorsDimensionOverrides(transport, binding);
    }
    public BusinessRepoConnectorsDimensionOverrides dimensionOverrides() { return dimensionOverrides; }
  }
  public static class BusinessRepoConnectorsDimensionOverrides extends BusinessRepoConnectorsDimensionOverridesResource {
    public BusinessRepoConnectorsDimensionOverrides(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessRequesters extends BusinessRequestersResource {
    public BusinessRequesters(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessReviewConfig extends BusinessReviewConfigResource {
    public BusinessReviewConfig(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessReviewDimensions extends BusinessReviewDimensionsResource {
    public BusinessReviewDimensions(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessRoles extends BusinessRolesResource {
    public BusinessRoles(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessTelemetry {
    protected final Transport transport;
    protected final String binding;
    private final BusinessTelemetryClients clients;
    public BusinessTelemetry(Transport transport, String binding) {
      this.transport = Objects.requireNonNull(transport); this.binding = binding;
      this.clients = new BusinessTelemetryClients(transport, binding);
    }
    public BusinessTelemetryClients clients() { return clients; }
  }
  public static class BusinessTelemetryClients extends BusinessTelemetryClientsResource {
    private final BusinessTelemetryClientsAllowedOrigins allowedOrigins;
    private final BusinessTelemetryClientsMoveTargets moveTargets;
    public BusinessTelemetryClients(Transport transport, String binding) {
      super(transport, binding);
      this.allowedOrigins = new BusinessTelemetryClientsAllowedOrigins(transport, binding);
      this.moveTargets = new BusinessTelemetryClientsMoveTargets(transport, binding);
    }
    public BusinessTelemetryClientsAllowedOrigins allowedOrigins() { return allowedOrigins; }
    public BusinessTelemetryClientsMoveTargets moveTargets() { return moveTargets; }
  }
  public static class BusinessTelemetryClientsAllowedOrigins extends BusinessTelemetryClientsAllowedOriginsResource {
    public BusinessTelemetryClientsAllowedOrigins(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessTelemetryClientsMoveTargets extends BusinessTelemetryClientsMoveTargetsResource {
    public BusinessTelemetryClientsMoveTargets(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessTickets extends BusinessTicketsResource {
    private final BusinessTicketsAssignableMembers assignableMembers;
    private final BusinessTicketsMessages messages;
    public BusinessTickets(Transport transport, String binding) {
      super(transport, binding);
      this.assignableMembers = new BusinessTicketsAssignableMembers(transport, binding);
      this.messages = new BusinessTicketsMessages(transport, binding);
    }
    public BusinessTicketsAssignableMembers assignableMembers() { return assignableMembers; }
    public BusinessTicketsMessages messages() { return messages; }
  }
  public static class BusinessTicketsAssignableMembers extends BusinessTicketsAssignableMembersResource {
    public BusinessTicketsAssignableMembers(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class BusinessTicketsMessages extends BusinessTicketsMessagesResource {
    public BusinessTicketsMessages(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class Feedback {
    protected final Transport transport;
    protected final String binding;
    private final FeedbackPosts posts;
    public Feedback(Transport transport, String binding) {
      this.transport = Objects.requireNonNull(transport); this.binding = binding;
      this.posts = new FeedbackPosts(transport, binding);
    }
    public FeedbackPosts posts() { return posts; }
  }
  public static class FeedbackPosts extends FeedbackPostsResource {
    public FeedbackPosts(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class Mailing extends MailingMailingResource {
    public Mailing(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class MailingServer {
    protected final Transport transport;
    protected final String binding;
    private final MailingServerEvents events;
    private final MailingServerSubscribers subscribers;
    public MailingServer(Transport transport, String binding) {
      this.transport = Objects.requireNonNull(transport); this.binding = binding;
      this.events = new MailingServerEvents(transport, binding);
      this.subscribers = new MailingServerSubscribers(transport, binding);
    }
    public MailingServerEvents events() { return events; }
    public MailingServerSubscribers subscribers() { return subscribers; }
  }
  public static class MailingServerEvents extends MailingServerMailingEventsResource {
    public MailingServerEvents(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class MailingServerSubscribers extends MailingServerMailingSubscribersResource {
    public MailingServerSubscribers(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class Root {
    protected final Transport transport;
    protected final String binding;
    private final RootAccount account;
    private final RootAnalytics analytics;
    private final RootAuth auth;
    private final RootBusinesses businesses;
    private final RootGithubApp githubApp;
    private final RootInvitations invitations;
    private final RootMailing mailing;
    private final RootPermissions permissions;
    private final RootTenantMerges tenantMerges;
    public Root(Transport transport, String binding) {
      this.transport = Objects.requireNonNull(transport); this.binding = binding;
      this.account = new RootAccount(transport, binding);
      this.analytics = new RootAnalytics(transport, binding);
      this.auth = new RootAuth(transport, binding);
      this.businesses = new RootBusinesses(transport, binding);
      this.githubApp = new RootGithubApp(transport, binding);
      this.invitations = new RootInvitations(transport, binding);
      this.mailing = new RootMailing(transport, binding);
      this.permissions = new RootPermissions(transport, binding);
      this.tenantMerges = new RootTenantMerges(transport, binding);
    }
    public RootAccount account() { return account; }
    public RootAnalytics analytics() { return analytics; }
    public RootAuth auth() { return auth; }
    public RootBusinesses businesses() { return businesses; }
    public RootGithubApp githubApp() { return githubApp; }
    public RootInvitations invitations() { return invitations; }
    public RootMailing mailing() { return mailing; }
    public RootPermissions permissions() { return permissions; }
    public RootTenantMerges tenantMerges() { return tenantMerges; }
  }
  public static class RootAccount extends RootAccountResource {
    public RootAccount(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class RootAnalytics extends RootAnalyticsResource {
    public RootAnalytics(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class RootAuth extends RootAuthResource {
    public RootAuth(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class RootBusinesses extends RootBusinessesResource {
    public RootBusinesses(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class RootGithubApp {
    protected final Transport transport;
    protected final String binding;
    private final RootGithubAppInstallUrl installUrl;
    private final RootGithubAppInstallations installations;
    public RootGithubApp(Transport transport, String binding) {
      this.transport = Objects.requireNonNull(transport); this.binding = binding;
      this.installUrl = new RootGithubAppInstallUrl(transport, binding);
      this.installations = new RootGithubAppInstallations(transport, binding);
    }
    public RootGithubAppInstallUrl installUrl() { return installUrl; }
    public RootGithubAppInstallations installations() { return installations; }
  }
  public static class RootGithubAppInstallUrl extends RootGithubAppInstallUrlResource {
    public RootGithubAppInstallUrl(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class RootGithubAppInstallations extends RootGithubAppInstallationsResource {
    public RootGithubAppInstallations(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class RootInvitations extends RootInvitationsResource {
    public RootInvitations(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class RootMailing {
    protected final Transport transport;
    protected final String binding;
    private final RootMailingReporting reporting;
    public RootMailing(Transport transport, String binding) {
      this.transport = Objects.requireNonNull(transport); this.binding = binding;
      this.reporting = new RootMailingReporting(transport, binding);
    }
    public RootMailingReporting reporting() { return reporting; }
  }
  public static class RootMailingReporting extends RootMailingReportingResource {
    public RootMailingReporting(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class RootPermissions extends RootPermissionsResource {
    public RootPermissions(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class RootTenantMerges extends RootTenantMergesResource {
    private final RootTenantMergesOptions options;
    public RootTenantMerges(Transport transport, String binding) {
      super(transport, binding);
      this.options = new RootTenantMergesOptions(transport, binding);
    }
    public RootTenantMergesOptions options() { return options; }
  }
  public static class RootTenantMergesOptions extends RootTenantMergesOptionsResource {
    public RootTenantMergesOptions(Transport transport, String binding) {
      super(transport, binding);
    }
  }
  public static class Telemetry extends TelemetryTelemetryResource {
    public Telemetry(Transport transport, String binding) {
      super(transport, binding);
    }
  }
}
