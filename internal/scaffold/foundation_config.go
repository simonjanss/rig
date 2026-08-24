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
		config("rig_tenant", "Tenant",
			`# Tenants are not tenant-scoped, so the generated queries have nothing to
# filter by and a generated CRUD interface would be an administrative
# back door. Manage them from a service you write.
expose: false`,
			`restore_window_days: 30`,
		),
		config("rig_identity", "Identity",
			`# An identity is global, so it has no tenant_id and the generated queries
# have nothing to scope by. Reaching one over HTTP would mean reading a
# person who works at another customer, so nothing here is exposed: the
# account is what a client sees, and it is tenant-scoped.
expose: false`,
			`restore_window_days: 30`,
		),
		config("rig_identity_credential", "IdentityCredential", notExposed),
		config("rig_identity_verification", "IdentityVerification", notExposed,
			`enums:
  rig_identity_verification_kind:
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
		config("rig_account", "Account",
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
  rig_account_kind:
    name: AccountKind
    description: What an account is.
    values:
      Person:
        name: Person
        description: Somebody who signs in. They have an identity, and the identity has the password.
      Service:
        name: Service
        description: What an integration's key acts as. It has no identity, so there is nothing to sign in with.
  rig_account_role_level:
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
	}
}

func sessionConfigs() []tableConfig {
	return []tableConfig{
		config("rig_account_token", "AccountToken", notExposed,
			`enums:
  rig_account_token_kind:
    name: AccountTokenKind
    description: What a token is for.
    values:
      Refresh:
        name: Refresh
        description: Exchanged for a new pair. It never authenticates a request.
      Access:
        name: Access
        description: Authenticates a request, and is short-lived because it travels.
  rig_account_token_client:
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
		config("rig_identity_session", "IdentitySession", notExposed),
		config("rig_auth_log", "AuthLog",
			`# Read-only. Entries are written by the auth package as things happen; an
# audit trail anybody can post to is not an audit trail.
operations: [Get, List, Search]`,
			`order_by: [-created_at, id]`,
			`enums:
  rig_auth_event:
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
  rig_auth_outcome:
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
	}
}

func apiKeyConfigs() []tableConfig {
	return []tableConfig{
		config("rig_api_key", "APIKey",
			`# Keys are minted and revoked through /auth/api-keys, not through CRUD.
# The secret exists only in the response that created it, and a generic
# POST could not return it.
expose: false`,
			`enums:
  rig_api_key_kind:
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
	}
}

func oauthConfigs() []tableConfig {
	return []tableConfig{
		config("rig_identity_oauth", "IdentityOAuth",
			`# Linking and unlinking go through the OAuth flow, which has to talk to the
# provider. A row created directly would claim an identity nobody verified.
expose: false`,
			`enums:
  rig_oauth_provider:
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
	}
}

// fileConfigs is rig_file's, and it is the one foundation table whose
// configuration is written for a project that wants it read rather than for one
// that wants CRUD.
//
// Read-only and narrow. A client needs the url to render a file and the name,
// type and size to describe it; the storage key, the checksum, the declared type
// and the tenant are the server's bookkeeping and never leave it. The storage
// key is the one that would actually matter — it is the thing a signed URL is
// built from, and syncing it is the same class of mistake as syncing a password
// hash.
//
// There is no write path to generate. The endpoints that put a file anywhere are
// the upload and the delete rig synthesizes against the row that owns it, and a
// client that could POST a rig_file row with an arbitrary key and no bytes has
// found a way around all of it.
func fileConfigs() []tableConfig {
	return []tableConfig{
		config("rig_file", "File",
			`# Read-only. Uploading is the nested endpoint on the row that owns the
# file, which is what makes the upload permissioned and tenant-scoped; a
# generic POST here would be a way around both.
operations: [Get, List]`,
			`# There is no restore_window_days here, and rig refuses one. How long a
# deleted file stays restorable is files.restore_window in rig.yaml: that
# number is how long the bytes are kept as well as how long the row can be
# brought back, and a second copy of it here could only disagree with it.
columns:
  storage_key:
    exclude: true
  checksum:
    exclude: true
  declared_content_type:
    exclude: true`,
		),
	}
}

// idempotencyConfigs is rig_idempotency's, and it is the shortest one here
// because the table is the least like a resource of anything the foundation
// creates.
//
// It exists to reserve the name. A project that exposed this table would be
// serving other people's stored responses over HTTP, so there is no
// configuration worth writing beyond saying no — but the resource name still has
// to be taken, or somebody's own `idempotency` table would project to it and the
// collision would surface as a generated method that overwrote another.
func idempotencyConfigs() []tableConfig {
	return []tableConfig{
		config("rig_idempotency", "Idempotency",
			`# Never exposed, and not the usual expose: false. The rows here are the
# stored responses of writes that carried an Idempotency-Key — somebody
# else's answers, kept only to be replayed to the caller that earned them.
# An endpoint over this table would serve them to anybody who could list it.
expose: false`,
		),
	}
}

// notificationConfigs are the two notification tables', and like rig_file's they
// are written for a project that wants them read rather than for one that wants
// CRUD.
//
// Neither needs a file for the inbox to work. The owner scope and the live-sync
// shape on rig_notification_recipient come from the `notifications:` block in
// rig.yaml, the way rig_file's restore window does, because they are one
// decision and a second copy of them here could only disagree. What a file adds
// is the rest of what a configuration asks for — the filter grammar, the sort
// keys, the generated client — for a project that turned `expose` on because the
// hand-written inbox routes were not enough.
func notificationConfigs() []tableConfig {
	return []tableConfig{
		config("rig_notification", "Notification",
			`# Read-only, and narrow. A notification is written by the engine and by
# nothing else: a client that could POST one could announce anything to
# anybody, and a client that could PATCH one could move a delivery date.
#
# What it is not is the inbox. This table holds rows that are pending for
# people who are not recipients yet and may never be, which is also why it
# has no live-sync shape.
operations: [Get, List]`,
		),
		config("rig_notification_device", "NotificationDevice",
			`# Where a push can reach somebody, and the one notification table a
# client genuinely writes to: registering a device is something the
# application it runs on does, and revoking one is something a person does
# from a list of their own devices.
operations: [Create, Get, List, Delete]`,
		),
		config("rig_notification_setting", "NotificationSetting",
			`# What somebody wants told to them, and when. A settings screen is a
# CRUD surface over exactly this, which is why it has one — and it is the
# only table here where a full one is the right answer.
operations: [Create, Get, List, Update, Delete]`,
		),
		config("rig_notification_delivery", "NotificationDelivery",
			`# Read-only, and narrow. A delivery row is the dispatcher's bookkeeping:
# retry counts, claim leases and provider errors. A client that could write
# one could send anything to anybody, and a client that could read them all
# would be reading a table that says who was told what.
#
# It is exposed at all because "why did I not get that mail" is a question
# support has to be able to answer.
operations: [Get, List]`,
		),
		config("rig_notification_recipient", "NotificationRecipient",
			`# The inbox. Read and delete, and nothing else: what a person may change
# about one of these is whether they have read it, and that is the
# _read endpoint rather than a PATCH that could rewrite the kind.
#
# The owner scope and the live-sync shape are not here. They come from the
# notifications block in rig.yaml, because they are the same decision for
# every project and a copy here could only disagree with it.
operations: [Get, List, Search, Delete]`,
		),
	}
}

// presenceConfigs is rig's presence table, as a project would configure it.
//
// One table, and the interesting half of this file is what is *not* here.
//
// There is no `access:` key. An owner scope is what a reader would reach for —
// presence is about a person, after all — and it would break the feature: on an
// owner-scoped table the generated shape carries `account_id = the caller's`
// before any application scope runs, and there is no `?scope=all` for a stream.
// A presence table with an owner streams every subscriber nothing but itself.
// The compiler settles that rather than this file, because it is the same answer
// in every project and a copy here could only ever disagree with it.
//
// There is no `electric:` key either, for the same reason and with the same
// consequence if it disagreed.
func presenceConfigs() []tableConfig {
	return []tableConfig{
		config("rig_presence", "Presence",
			`# Read-only, and only barely. The live shape is how a browser actually
# reads this table; Get and List are here because "who is here" is a question
# a server-side caller and a diagnostic page also ask, and because an empty
# operations list does not mean what it looks like — it reads as
# "unspecified" and falls back to the full CRUD set.
#
# Not Create, not Update, not Delete. The routes under /presence are the
# whole write surface: they take the account from the credential rather than
# from a body, so "you may only write your own presence" is a sentence a
# client cannot phrase rather than a rule somebody enforces. A generated
# Create would take a body, and a body is somewhere to name somebody else.
operations: [Get, List]`,
		),
	}
}
