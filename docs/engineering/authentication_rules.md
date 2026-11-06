# Authentication rules: ninzo-backend

Version: 0.2.0 · Updated: 2026-11-06 · Discussion: Issue #25

The record of how people sign in, what roles they hold, and why. Read this before changing sign-in, sessions, roles or the `identity` module. If a rule here and the contracts disagree, the contracts win for what is built today, and the difference is a bug to raise as an Issue. Section 11 lists the contract changes already planned.

**Build status:** the tables and the session system (section 4) are built. Section 11 says what comes when.

**How to read the labels:**
- **Owner** means the owner decided it, and it must not be changed or asked again without the owner. The wording the owner approved is on Issue #25.
- **Proposed** means an engineering default the owner has not ruled on. It can change with a reason recorded here.
- **Open** means nobody has decided yet.

## 1. The rules in short
1. One person holds one account, and that account can have many roles. **Owner**
2. Each session is for exactly one role. **Owner**
3. Only registered users can sign in, and this is checked at sign-in. **Owner**
4. Session handling is the same system for every role. **Owner**
5. Riders stay signed in through normal work. **Owner**
6. Customers must verify a phone before their first order. **Owner**
7. A customer has one sign-in method at a time, and accounts never merge: the way you signed up is the way you log in, until you change it on purpose. **Owner**, decision 0009
8. Services use a separate database login from `postgres`, which stays for migrations only. **Owner**

## 2. Sign-in options by user type
**Owner**, except where marked.

| User | Identify by | Prove with | Who creates the account |
|---|---|---|---|
| Customer | Phone number (main), then Google and Apple | OTP, or the Google or Apple token | The customer, at first sign-in |
| Rider | Phone number only | OTP only | The rider applies, and Ninzo approves (section 6) |
| Partner | Phone or partner ID | OTP or password | Ninzo |
| Admin | Phone or staff ID | OTP or password, plus an authenticator app, always | Ninzo |
| Help and support | Phone or staff ID | OTP or password, plus an authenticator app by default | Ninzo |

**Authenticator:** always required for admin, because admin can act on money. For support it is on by default, and a console setting can turn it off, with SMS OTP as the fallback second step. **Owner**

With that fallback, support signs in with a password first and then SMS OTP. OTP then OTP would be one proof used twice. **Proposed**

**Order of building:** phone OTP and Google are built together. Apple comes last. **Owner**, 2026-11-06. Phone is the main sign-in because phone numbers are very accessible in India.

**Riders:** phone and OTP only. If a phone is lost, other options are decided later. **Owner**, 2026-11-06, replacing the earlier decision of a rider ID and password. The `rider_code` column and `rider_credentials` table stay, unused, because applied migrations are not edited and those options may need them.

### Why passwords exist for partners and staff
They keep working when SMS does not, and each password sign-in saves a paid SMS.

**Accepted risk for riders:** during an SMS outage, a rider whose session has ended cannot sign in. Sessions last 90 days and renew with use, so a working rider should not normally meet this. Weigh it when deciding the phone-lost options.

### Why customers have no password
They have Google, Apple and OTP. A customer password would add a reset flow and a store of secrets for no gain.

## 3. Roles and data model
**Owner** approved the design. Table details are **Proposed**.

```
Person: phone, Google and Apple identities, overall status
  ├─ Customer profile   created by the person's first customer sign-in
  ├─ Rider profile      created when they apply, usable once approved
  ├─ Partner profile    created by Ninzo (v0.8.0)
  └─ Staff profile      created by Ninzo, as admin or support (v0.9.0)
```

- `people` has one row per person. It holds the phone, which is unique, and can be empty until verified.
- A Google or Apple identity belongs to a person. The pair of provider and provider user ID is unique.
- `customers` and `riders` stay as role profiles, each with a unique `person_id`.
- Orders, offers and tokens keep pointing at the role profile, so other modules do not need to know about `people`.
- A role exists only if its profile exists. There is no separate list of roles to keep in step.

### Options considered
| Option | Why not, or why |
|---|---|
| A. Separate accounts per role, the first schema | The same phone sits in several tables, so a phone change or a fraud ban has to find every copy |
| B. One table holding everything, with a role list | Mixes rider, customer and staff data, and means rewriting the data map |
| **C. One person plus role profiles. Chosen** | One extra table, and the other modules stay as they are |

## 4. Sessions
**Owner:** the same system for every role, and riders stay signed in. Numbers are **Proposed**, except the access token length, which is in the contract.

- Every sign-in method ends the same way: we issue **our own** access and refresh tokens. Only the first step differs (section 5).
- A token records its user type and role profile. A token for one role is refused on another role's APIs.
- Access tokens last 15 minutes, and the app renews them silently.
- Refresh tokens are single use, and each refresh returns a new pair. They are stored only as hashes.

| Role | Refresh token life | Why |
|---|---|---|
| Customer | 90 days, restarting at each use | Convenience |
| Rider | 90 days, restarting at each use | A working rider should never see a sign-in screen |
| Partner | 30 days, restarting at each use | Shared devices at hubs are more likely |
| Admin and support | 12 hours, with no restart | They can do the most damage |

### When a rider is signed out
Only when they sign out, Ninzo suspends the role, a token looks stolen, their phone changes, or the app goes unused for 90 days. Never because of a weak network, or because a shift ended.

### Lost replies and stolen tokens
- A client sends one refresh at a time, and other requests wait for it.
- Each renewal is one generation further along the chain. A token is good until a token of a later generation has been used, or another token of its own generation has. **Proposed**
- So a used token can come back, however long ago, and get a new pair. A lost reply or a delayed duplicate never signs anyone out: the extra tokens are simply never used.
- If two different tokens of one generation are both used, or an old token comes back after a later one was used, there are two holders of the session, so one may be a thief. The chain ends and the user signs in again, including the thief.
- **Accepted risk:** a stolen token works undetected until the real app next renews, so while the app is idle a thief can use it. Tying tokens to a device would close this, and can be added later. **Proposed**
- **The one exception:** a duplicate delayed until after the app's next renewal looks the same as theft. That needs a request stuck for about 15 minutes, past every timeout.
- A database outage returns 503, never 401, so an outage cannot sign anyone out.
- Everything that changes a chain takes turns, so ending a chain cannot miss a token a refresh in progress is making.
- An app that gets a 401 on a signed-in request refreshes once and sends it again. Only a 401 from the refresh means sign in, so replacing the signing key signs nobody out.
- Signing out, or a reset, ends the refresh token at once. An access token already issued works for up to 15 minutes, so endpoints with sensitive actions must also check status when they are built.
- Queued offline work is never lost because a session needed renewing.

Revoking only the chain, not all the user's tokens, is **Proposed**, so that one device's problem does not sign out another.

### Suspension
- Tokens for the suspended role are revoked at once. A person-level block revokes every role's tokens. An access token already issued can last up to 15 minutes, so status is also checked at refresh and on sensitive actions. **Proposed**

## 5. How each sign-in method works
### Phone OTP
The backend makes a six-digit code, sends it, stores only a keyed hash, and checks it. A code lasts 5 minutes, and allows 5 attempts. A phone can ask once every 30 seconds per app, 20 times an hour per app, and 5 times an hour per app from one network address. Only a code really made counts toward the 5. **Proposed**

Only Indian mobile numbers are accepted for the pilot, because a gateway that strips a leading zero could reach one person through two numbers, and foreign numbers invite SMS pumping. **Proposed**

A failed send frees the limit. In test mode the code is fixed and no SMS goes out, and production refuses test mode. Real SMS is not built yet, so production cannot start until it is. That is deliberate.

**Test mode opens any number with the fixed code, and reveals registered riders. So staging must hold test data only, and never real customers.**

**For the real SMS step:** say in the message what a code is for, because a code that links an account must warn that it does. That needs a second DLT template. Send through the outbox and always reply 200. Sending during the request takes longer for a registered number, and a failed send replies differently, and either one reveals who is registered.

Each code is tied to one purpose, such as sign-in or applying as a rider, so a code sent for one cannot be used for another. A phone verification code works only in the session that asked for it. **Proposed**

### Google
The app gets an ID token from Google and sends it to the backend. The backend checks the signature against Google's keys, plus the issuer, audience and expiry. Google's keys are cached, and an outage returns 503, never a refusal. **Proposed**

### Apple
The same, with Apple's identity token. Apple's refresh token is also kept, encrypted, only so it can be revoked when an account is deleted. **Proposed**

### Why not exchange an authorization code with Google
The owner first described the backend exchanging an authorization code. That flow is needed only to call Google's services on the user's behalf. We only need to know who the user is, so an ID token does the job, with no client secret and no Google tokens to store. The result is the same. Only Apple needs a code exchange, for the deletion rule above.

## 6. Registration
**Owner**

| User | Rule |
|---|---|
| Customer | Registers by signing in: the first verified sign-in creates the customer profile |
| Rider | Anyone can apply, and only Ninzo can approve. Applying has its own OTP path (**Proposed**) |
| Partner, admin, support | Ninzo creates the account. They cannot register |

### What an unregistered person sees
This covers **worker sign-in**. Customers register by signing in, and rider applicants use the application path below. No SMS is sent, which costs nothing. The screen says "if this number is registered, you'll get a code", and a failed sign-in never says whether the ID exists. Otherwise anyone could find out which numbers belong to riders or staff. The owner approved that a failed sign-in never reveals whether the ID exists. Sending no SMS, and this screen wording, are **Proposed**.

**Keeping the replies identical. Proposed:** an unknown rider number gets a real request row, with a code nobody knows and no SMS, so wrong guesses, expiry and limits give the same errors as a real request. Blocked or suspended is revealed only after a correct code.

### Rider application
```
Customer taps "Become a rider", and signs in to the rider app with the same phone
  → Applicant: can only fill in details and see progress
  → Documents verified, and requirements met
  → Ninzo approves
  → Active rider: can start shifts
```
- The application sends an OTP to any phone, even a new one, so that a new person can apply. It is rate limited, because each SMS costs money. A phone already registered as a rider gets the same reply. **Proposed**
- `riders.status` gains `applicant`, and the database must allow only known values. Its default today is `active`, so a new row could fail open: the default must become `applicant`. `name` must be allowed to be empty for an applicant. **Proposed**
- An applicant can never start a shift or see orders, because riders handle sealed food at customers' gates. **Owner** `startShift` needs a new error for this, `rider_not_approved`, in the contract update.
- KYC and documents go through DigiLocker, later. **Owner**
- Part-time means no fixed hours, which `startShift` and `endShift` already allow.
- Until the console exists (v0.9.0), approval is an internal script with an audit record. **Proposed**

### Passwords, for partners and staff
**Owner**
1. The first sign-in is by OTP, and then they set a password.
2. A forgotten password is reset by OTP. If the phone is lost too, support resets it after checking who they are.
3. Passwords are stored only as strong hashes, attempts are limited, and a failed sign-in never reveals whether the ID exists.
4. Each role has its own password, so a weak partner password cannot open a staff account.

**Limits on resets. Proposed:**
- **Owner**, 2026-11-05: support can reset passwords only for partners. A staff password is reset only by an admin, and nobody can reset their own this way. Otherwise a support agent could take over an admin account.
- If the only admin is locked out, an audited internal script does the reset.
- Every reset is audited, and revokes all that role's tokens, so a stolen token does not survive it.

## 7. Conflict rules
**Owner**

| Situation | Rule | Why |
|---|---|---|
| Rider orders food, off duty | Allowed, through the customer app | Their rider role is not affected |
| Rider orders food, on duty | Allowed, and never offered their own order | Stops them earning pay or a late credit on it |
| Delivery offers | Only to riders on an open shift | Food waits only in a scooter on shift, and the timer depends on that |
| Staff with their own order | Cannot act on their own orders, refunds, tickets or credits. Someone else must | Conflict of interest |
| A suspended rider orders food | Allowed: suspension is per role | They are barred from riding, not from buying |
| Serious fraud | A person-level block stops every role | Fraud follows the person |
| A customer deletes their account | Only the customer role is removed. Rider and payment records stay | The law may require those records |
| Phone change | Done once on the person, after OTP on the new number. Riders and staff also need a check by support | It moves every role, and a phone is how accounts are proven |

**Limits on phone changes. Proposed:**
- **Owner**, 2026-11-05: a staff member's change is checked by an admin, and never by themselves.
- Partners need the same support check as riders.
- A new number that already belongs to another person is refused.
- The change revokes all that person's tokens, and tells the old number.

## 8. Situations
| Situation | What happens |
|---|---|
| A rider starts ordering food | Signs in to the customer app with the same phone. A customer profile is created |
| A customer becomes a rider | Applies. A rider profile is added to the same person, and nothing is copied |
| A customer tries the rider app with no application | Offered the application. No rider data or orders are shown |
| Support is also a rider | One person with two separate sessions |
| Google sign-in for a new person | A person with no phone, and a customer profile. Until the phone is verified they can only view the menu and fill a cart, which stays in the app: orders, gates, notify requests and tickets return `phone_required`, and availability needs a saved gate. So an unverified profile holds nothing that a move would lose, except its name |
| When a Google customer is asked for a phone | When they go to order, not straight after sign-in, because the rider and support need a working number by then. The app keeps the cart through verification. **Owner** |
| A Google customer's cart may not be in reach | They choose from the full menu, and availability needs a saved gate, so they learn what is in reach only after verifying a phone. Checking a location without saving it would fix this: **Open**, for `v0.3.0` |
| An SMS outage | People already signed in are not affected. Partners and staff can use passwords. New customers can use Google or Apple, but cannot verify a phone until it returns. A rider whose session has ended cannot sign in |
| A Google or Apple outage | Those users can switch to phone at sign-in, if their phone is verified. That change stays until they change back |
| Google sign-in, then the number has another account | Refused with `phone_already_registered`, for a customer or a rider. Nothing joins. They log in with phone and OTP |
| A Google customer uses phone and OTP | After a correct code: `sign_in_method_mismatch`, and the app offers to switch. Agreeing keeps the account, and removes the Google link |
| A customer loses their Google account | As above, with their verified phone. With no verified phone they have no orders, and start again |
| A phone customer wants Google, or the reverse | `changeSignInMethod` in settings, while signed in, after proving the new method |
| Changing to a Google account held by an empty account | The empty account is removed. If it has a phone, a rider profile or a block, this is refused with `google_account_in_use` |
| A customer adds an email in their profile | Contact details only, never a way to log in. It is not unique, and not verified |
| A customer with a verified phone verifies a different number | Refused with `phone_already_set`. Changing a phone is a separate flow, because it moves every role |
| The reply to `verifyPhone` is lost | A retry returns 401 or `invalid_otp`, because the old access token can last 15 minutes. They sign in again, and the phone is already verified |
| A weak network during renewal | The retry returns a valid pair, and the rider is not signed out |

**One sign-in method. Owner, decision 0009:**
- The method is `sign_in_method` on `customers`, set at sign-up. It is per customer, because riders always use a phone.
- Changing it removes the old Google link and every device, ends every session, tells the phone, and is recorded in the audit log, with the old Google account named.
- A correct code is needed before the reply reveals a customer's method, so only the phone's holder learns it.
- The customer is always told what happened, so a change of method never looks like a fault with the OTP. **Owner.** The suggested wording is in `contracts/README.md`.
- Each method is a component, switched on with `SIGNIN_METHODS`. The rule itself lives in `internal/identity/method.go`.

## 9. Problems found, and how they were solved
| Problem | Solution |
|---|---|
| Google and Apple do not reliably give a phone number, but delivery contact needs one | Phone OTP is required before the first order, so OTP is needed even with Google |
| `customers.phone` and `riders.phone` were required, and already applied to Supabase | A new migration moves them to `people`, and allows the phone to be empty. Applied migration files are never edited. The tables held no rows |
| Checking registration could reveal who is registered | The check is on the server, and the reply is the same either way |
| "Ninzo creates every rider" conflicted with customers doing part-time work | Riders can apply themselves, and approval stays with Ninzo |
| Part-time riders could outnumber the pilot's 4-8 scooters | Left to the business. The backend must not assume who supplies scooters. The contract already has `shift_limit_reached`, so the check exists, as a console setting that is off by default. **Proposed** |
| A lost reply could have signed riders out on weak networks | The retry rule in section 4 |
| A stored reply could have held live tokens in plain text | Replies that carry tokens are never stored, and stored replies are stripped of secrets |
| SMS outages could stop mid-shift sign-in | Long sessions that renew with use. Partners and staff also have passwords |

### Providers considered
Checked on 2026-11-05. Prices came mostly from vendor blogs and comparison sites, so confirm them before relying on them.

| Option | Decision | Why |
|---|---|---|
| Our own OTP | **Proposed**. The owner decided on OTP as an option, and not how it is built | Fits the existing contract and tables |
| MSG91 to send the SMS | **Proposed**. The owner has not named a provider | About ₹0.15-0.25 per SMS. The provider can be swapped |
| Clerk | Not used | India SMS is off by default, SMS is not on the free plan, and a native Flutter SDK could not be confirmed. Would duplicate work already done |
| Firebase phone auth | Not used | About $0.07 per SMS in India, and it moves sign-in into the apps |

MSG91 is not free: it charges per message. Development uses test OTPs at no cost. No uptime figure or SLA was found for MSG91, so ask for one in writing. Indian SMS needs DLT registration, which takes time, so it should start early.

**Test OTPs must never work in production.** The backend should refuse to start if they are on in production. **Proposed**

## 10. Open points
- **Scooter supply for part-timers, and any shift limit:** business decisions. **Owner** left these to the business.
- **Requirements before approval:** KYC and documents through DigiLocker are confirmed for later. Food safety training and payout details were suggested, and need detail. Nothing stores payout details yet.
- **Suspended during a delivery:** does the current order finish, or get reassigned? **Open.** Proposed: ops decides, and a safety case reassigns at once.
- **A rider whose phone is lost:** other options are decided later. **Owner**
- **Lockout by someone else's requests:** limited by counting 5 codes an hour per phone and network address, with 20 an hour per phone in all. Decision 0010, for owner review. See `known_downsides.md`.
- **Guessing odds:** 5 attempts on each code. Each network address gets 200 verify attempts an hour across all numbers, which caps guessing from one address. Review the odds before launch, since a botnet has many addresses.
- **Recycled phone numbers:** a telecom can give an old number to someone new, who could then prove that phone. **Open.** Needs a rule before launch, especially for accounts with credit or history.
- **Apple on iOS:** from memory, App Store rules require an option like Apple sign-in if an iOS app offers Google. Apple's developer program costs about $99 a year. Confirm both. If correct, Apple must be ready before the iOS release.
- **Going live with Google:** from memory, the Google project starts in testing mode, limited to 100 test users, and must be switched to production before launch. Confirm in the console.

## 11. What comes when
| Version | Work |
|---|---|
| `v0.2.0` | `people`, the identity table, the `applicant` status, the OTP purpose, a separate service database login. Contract and mock updates, including `rider_not_approved`, and revoking only the chain. Session system. Customer Google and phone verification. Rider phone and OTP sign-in. Test OTPs until MSG91 and DLT are ready |
| `v0.3.0` | The rider application flow, with rider profiles |
| Before the iOS release | Apple sign-in |
| `v0.8.0` | Partner sign-in. `refresh_tokens`, `otp_requests` and `devices` are widened to allow the new user type |
| `v0.9.0` | Staff sign-in, roles and the authenticator. The same tables are widened |

## 12. Related documents
- Sign-in rules for apps: `contracts/README.md`
- Tables: `docs/engineering/endpoint_data_map.md`
- Every downside we accepted: `known_downsides.md`
- Why one sign-in method: `docs/decisions/0009-one-sign-in-method.md`
- Discussion and history: Issue #25
