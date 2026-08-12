package scaffold

// The foundation's table configuration.
//
// Column comments are absent on purpose: the migrations carry COMMENT ON for
// every column, so they arrive through introspection and there is one place to
// edit them. What is here is the intent Postgres cannot express — whether a
// table belongs in the API, how long a deleted row stays restorable, and what
// each enum value means.

func tenancyConfigs() []tableConfig {
	return []tableConfig{
		{
			table: "tenant",
			content: config("tenant", schemaRef,
				`# Tenants are not tenant-scoped, so the generated queries have nothing to
# filter by and a generated CRUD interface would be an administrative
# back door. Manage them from a service you write.
expose: false`,
				`restore_window_days: 30`,
			),
		},
		{
			table: "identity",
			content: config("identity", schemaRef,
				`# An identity is global, so it has no tenant_id and the generated queries
# have nothing to scope by. Reaching one over HTTP would mean reading a
# person who works at another customer, so nothing here is exposed: the
# account is what a client sees, and it is tenant-scoped.
expose: false`,
				`restore_window_days: 30`,
			),
		},
		{
			table:   "identity_credential",
			content: config("identity_credential", schemaRef, notExposed),
		},
		{
			table: "identity_verification",
			content: config("identity_verification", schemaRef, notExposed,
				`enums:
  identity_verification_kind:
    name: IdentityVerificationKind
    description: What a single-use link is for.
    values:
      EmailVerification:
        name: EmailVerification
        description: Confirms that an address belongs to the person who gave it.
      PasswordReset:
        name: PasswordReset
        description: Lets somebody set a new password without knowing the old one.
      Invitation:
        name: Invitation
        description: Brings a person into a tenant, whether or not they already have an identity.`,
			),
		},
		{
			table: "account",
			content: config("account", schemaRef,
				`# Everything but Create. An account created through plain CRUD would have
# no identity behind it and no invitation sent, so joining a tenant is an
# auth endpoint rather than a POST anyone can make.
operations: [Get, List, Search, Update, Delete]`,
				`restore_window_days: 30`,
				`order_by: [-created_at, id]`,
				`columns:
  identity_id:
    # Which person this is. Decided when the account is created and never
    # afterwards: moving an account to another person would hand over
    # everything they have ever written.
    operations: [Read]
  email_address:
    # A copy of the identity's address. Changing it is a verification flow on
    # the identity, not a field edit here.
    operations: [Read]
    format: EmailAddress
  kind:
    # A person does not become a service account, and a service account has no
    # password to make it a person. Which it is, is decided when it is created.
    operations: [Read]
  role:
    # The coarse level. Who may raise somebody to Owner is your rule to write —
    # a hook on this table is the place for it — and rig will not invent one.
    operations: [Read, Update]
  time_zone:
    # Whatever the person set it to. An unknown zone is not worth refusing a
    # write over: the account package falls back to UTC when it cannot load one.
    operations: [Read, Create, Update]`,
				`enums:
  account_kind:
    name: AccountKind
    description: What an account is.
    values:
      Person:
        name: Person
        description: Somebody who signs in. They have an identity, and the identity has the password.
      Service:
        name: Service
        description: What an integration's key acts as. It has no identity, so there is nothing to sign in with.
  account_role_level:
    name: AccountRoleLevel
    description: The coarse level an account holds in one tenant.
    values:
      Owner:
        name: Owner
        description: May do anything, including the things that end the tenant.
      Admin:
        name: Admin
        description: Administers the tenant without the decisions that are the owner's to make.
      Basic:
        name: Basic
        description: Gets on with the work.`,
			),
		},
	}
}

func sessionConfigs() []tableConfig {
	return []tableConfig{
		{
			table: "account_token",
			content: config("account_token", schemaRef, notExposed,
				`enums:
  account_token_kind:
    name: AccountTokenKind
    description: What a token is for.
    values:
      Refresh:
        name: Refresh
        description: Exchanged for a new pair. It never authenticates a request.
      Access:
        name: Access
        description: Authenticates a request, and is short-lived because it travels.
  account_token_client:
    name: AccountTokenClient
    description: What kind of thing holds a session.
    values:
      Web:
        name: Web
        description: A browser.
      Mobile:
        name: Mobile
        description: An application on a phone or tablet.
      Machine:
        name: Machine
        description: An integration acting through an API key.`,
			),
		},
		{
			table:   "identity_session",
			content: config("identity_session", schemaRef, notExposed),
		},
		{
			table: "auth_log",
			content: config("auth_log", schemaRef,
				`# Read-only. Entries are written by the auth package as things happen; an
# audit trail anybody can post to is not an audit trail.
operations: [Get, List, Search]`,
				`order_by: [-created_at, id]`,
				`enums:
  auth_event:
    name: AuthEvent
    description: What happened.
    values:
      LoginAttempted:
        name: LoginAttempted
        description: A sign-in was tried. Recorded before the password is checked.
      LoginSucceeded:
        name: LoginSucceeded
        description: A sign-in worked. It clears the failure window for that address.
      LoginFailed:
        name: LoginFailed
        description: A sign-in did not work. This is what the lockout counts.
      AccountLocked:
        name: AccountLocked
        description: Too many failures, so further attempts are refused for a while.
      Logout:
        name: Logout
        description: A session was ended deliberately.
      TokenRefreshed:
        name: TokenRefreshed
        description: A refresh token was exchanged for a new pair.
      TokenReuseDetected:
        name: TokenReuseDetected
        description: A consumed refresh token was presented again, so its family was revoked.
      PasswordResetRequested:
        name: PasswordResetRequested
        description: Somebody asked for a reset link.
      PasswordResetCompleted:
        name: PasswordResetCompleted
        description: A reset link was used to set a new password.
      PasswordChanged:
        name: PasswordChanged
        description: Somebody who knew their password set a new one.
      EmailVerified:
        name: EmailVerified
        description: An address was confirmed.
      VerificationResent:
        name: VerificationResent
        description: A verification link was sent again.
      ApiKeyAuthSucceeded:
        name: APIKeyAuthSucceeded
        description: A machine authenticated with a key.
      ApiKeyAuthFailed:
        name: APIKeyAuthFailed
        description: A key was presented that could not be used.
      ImpersonationStarted:
        name: ImpersonationStarted
        description: An administrator began acting as somebody else.
      ImpersonationEnded:
        name: ImpersonationEnded
        description: An administrator stopped acting as somebody else.
      OAuthSignIn:
        name: OAuthSignIn
        description: Somebody signed in through an external provider.
      AccountProvisioned:
        name: AccountProvisioned
        description: An account was created in a tenant, by a person or by an integration's key.
      InvitationSent:
        name: InvitationSent
        description: Somebody was invited into a tenant and a single-use link was minted.
      InvitationAccepted:
        name: InvitationAccepted
        description: An invitation was redeemed. It confirms the address and, for a first account, sets the password.
      InvitationRevoked:
        name: InvitationRevoked
        description: An invitation was withdrawn before it was used, so the link stopped working and the account it was for was removed.
      TenantSwitched:
        name: TenantSwitched
        description: Somebody moved to another tenant they belong to, which issues a session for that tenant's account.
  auth_outcome:
    name: AuthOutcome
    description: Whether an attempt worked.
    values:
      Succeeded:
        name: Succeeded
        description: It worked.
      Failed:
        name: Failed
        description: It did not.`,
			),
		},
	}
}

func apiKeyConfigs() []tableConfig {
	return []tableConfig{
		{
			table: "api_key",
			content: config("api_key", schemaRef,
				`# Keys are minted and revoked through /auth/api-keys, not through CRUD.
# The secret exists only in the response that created it, and a generic
# POST could not return it.
expose: false`,
				`enums:
  api_key_kind:
    name: APIKeyKind
    description: Who a key acts as.
    values:
      Integration:
        name: Integration
        description: A key with a service account of its own, so what it writes is attributed to the integration rather than to whoever set it up.
      Personal:
        name: Personal
        description: A key that acts as the person who made it, for somebody automating their own work.`,
			),
		},
	}
}

func oauthConfigs() []tableConfig {
	return []tableConfig{
		{
			table: "identity_oauth",
			content: config("identity_oauth", schemaRef,
				`# Linking and unlinking go through the OAuth flow, which has to talk to the
# provider. A row created directly would claim an identity nobody verified.
expose: false`,
				`enums:
  oauth_provider:
    name: OAuthProvider
    description: An external identity provider.
    values:
      Google:
        name: Google
        description: Google, including Tenant accounts.
      Microsoft:
        name: Microsoft
        description: Microsoft, including Entra ID accounts.
      GitHub:
        name: GitHub
        description: GitHub.`,
			),
		},
	}
}
