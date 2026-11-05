# Authentication rules: ninzo-backend

Version: 0.1.0 · Updated: 2026-11-05 · Discussion: Issue #25

The record of how people sign in, what roles they hold, and why. Read this before changing sign-in, sessions, roles or the `identity` module. If a rule here and the contracts disagree, the contracts win for what is built today, and the difference is a bug to raise as an Issue. Section 11 lists the contract changes already planned.

**Build status:** nothing here is built yet. Section 11 says what comes when.

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
7. Accounts merge only on a phone proven by OTP, never on matching email. **Owner**
8. Services use a separate database login from `postgres`, which stays for migrations only. **Owner**

## 2. Sign-in options by user type
**Owner**, except where marked.

| User | Identify by | Prove with | Who creates the account |
|---|---|---|---|
| Customer | Phone, Google or Apple | OTP, or the Google or Apple token | The customer, at first sign-in |
| Rider | Phone or rider ID | OTP or password | The rider applies, and Ninzo approves (section 6) |
| Partner | Phone or partner ID | OTP or password | Ninzo |
| Admin | Phone or staff ID | OTP or password, plus an authenticator app, always | Ninzo |
| Help and support | Phone or staff ID | OTP or password, plus an authenticator app by default | Ninzo |

**Authenticator:** always required for admin, because admin can act on money. For support it is on by default, and a console setting can turn it off, with SMS OTP as the fallback second step. **Owner**

With that fallback, support signs in with a password first and then SMS OTP. OTP then OTP would be one proof used twice. **Proposed**

**Order of building:** Google comes first, for the development phase. OTP and Apple follow on the same system. **Owner**

### Why passwords exist for workers
They keep working when SMS does not. A rider in the middle of a shift can still sign in during an SMS provider outage, and each password sign-in also saves a paid SMS.

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
Only when they sign out, Ninzo suspends the role, a token looks stolen, their password is reset, their phone changes, or the app goes unused for 90 days. Never because of a weak network, or because a shift ended.

### Lost replies and stolen tokens
- A client sends one refresh at a time, and other requests wait for it.
- If a used token comes back and the pair issued from it has not been used, the reply was probably lost, so the server returns a valid pair, however long ago it was. A rider can lose signal for a long time, and must not be signed out for it.
- **Accepted risk:** a stolen old token also works in that case, until the real app next renews. A 60-second limit was tried and rejected, because it signed out riders who lost signal. Tying retries to a device would close this, and can be added later. **Proposed**
- If that pair has been used, the token may be stolen, so that chain is revoked and the user signs in again.
- Queued offline work is never lost because a session needed renewing.

Revoking only the chain, not all the user's tokens, is **Proposed**, so that one device's problem does not sign out another.

### Suspension
- Tokens for the suspended role are revoked at once. A person-level block revokes every role's tokens. An access token already issued can last up to 15 minutes, so status is also checked at refresh and on sensitive actions. **Proposed**

## 5. How each sign-in method works
### Phone OTP
The backend makes a code, sends it, stores only a hash, and checks it. Attempts and sends are rate limited.

Each code is tied to one purpose, such as sign-in, applying as a rider, or resetting a password, so a code sent for one cannot be used for another. `otp_requests` needs a purpose column. **Proposed**

### Google
The app gets an ID token from Google and sends it to the backend. The backend checks the signature against Google's keys, plus the issuer, audience and expiry. **Proposed**

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

### Rider application
```
Customer taps "Become a rider", and signs in to the rider app with the same phone
  → Applicant: can only fill in details and see progress
  → Documents verified, and requirements met
  → Ninzo approves
  → Active rider: gets a rider ID, sets a password, can start shifts
```
- The application sends an OTP to any phone, even a new one, so that a new person can apply. It is rate limited, because each SMS costs money. A phone already registered as a rider gets the same reply. **Proposed**
- `riders.status` gains `applicant`, and the database must allow only known values. Its default today is `active`, so a new row could fail open: the default must become `applicant`. `name` must be allowed to be empty for an applicant. **Proposed**
- An applicant can never start a shift or see orders, because riders handle sealed food at customers' gates. **Owner** `startShift` needs a new error for this, `rider_not_approved`, in the contract update.
- KYC and documents go through DigiLocker, later. **Owner**
- Part-time means no fixed hours, which `startShift` and `endShift` already allow.
- Until the console exists (v0.9.0), approval is an internal script with an audit record. **Proposed**

### Passwords
**Owner**
1. The first sign-in is by OTP, and then they set a password. A rider sets one only once approved.
2. A forgotten password is reset by OTP. If the phone is lost too, support resets it after checking who they are.
3. Passwords are stored only as strong hashes, attempts are limited, and a failed sign-in never reveals whether the ID exists.
4. Each role has its own password, so a weak rider password cannot open a staff account.

**Limits on resets. Proposed:**
- **Owner**, 2026-11-05: support can reset passwords only for riders and partners. A staff password is reset only by an admin, and nobody can reset their own this way. Otherwise a support agent could take over an admin account.
- If the only admin is locked out, an audited internal script does the reset.
- Every reset is audited, and revokes all that role's tokens, so a stolen 90-day token does not survive it.

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
| Google sign-in for a new person | A person with no phone, and a customer profile. They can browse. Phone verification comes before the first order |
| Google sign-in, then the phone proves to be an existing person | The Google identity moves to that person, and the new person is removed. It cannot have orders, because ordering needs a verified phone. See the rules below |
| That existing person already has a different Google identity | Refused with a clear error, asking them to sign in with OTP. **Proposed** |
| An SMS outage | People already signed in are not affected. Riders, partners and staff can use passwords. New customers can use Google or Apple, but cannot verify a phone until it returns |
| A Google or Apple outage | Those users can sign in with phone OTP, if their phone is verified |
| A weak network during renewal | The retry returns a valid pair, and the rider is not signed out |

**Moving a Google identity. Proposed:**
- If the existing person has no customer profile, such as a rider-only person, the new customer profile moves to them. Otherwise the empty one is removed.
- Tokens for a removed profile are revoked in the same transaction. `refresh_tokens` and `devices` have no foreign key to profiles, so nothing else would stop them working.
- A blocked person is refused.
- An SMS tells the phone that a Google account was linked. One intercepted OTP could otherwise link an attacker's Google for good, without anyone knowing.

## 9. Problems found, and how they were solved
| Problem | Solution |
|---|---|
| Google and Apple do not reliably give a phone number, but delivery contact needs one | Phone OTP is required before the first order, so OTP is needed even with Google |
| `customers.phone` and `riders.phone` were required, and already applied to Supabase | A new migration moves them to `people`, and allows the phone to be empty. Applied migration files are never edited. The tables held no rows |
| Checking registration could reveal who is registered | The check is on the server, and the reply is the same either way |
| "Ninzo creates every rider" conflicted with customers doing part-time work | Riders can apply themselves, and approval stays with Ninzo |
| Part-time riders could outnumber the pilot's 4-8 scooters | Left to the business. The backend must not assume who supplies scooters. The contract already has `shift_limit_reached`, so the check exists, as a console setting that is off by default. **Proposed** |
| A lost reply could have signed riders out on weak networks | The retry rule in section 4 |
| SMS outages could stop mid-shift sign-in | The password option for workers |

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
- **Recycled phone numbers:** a telecom can give an old number to someone new, who could then prove that phone. **Open.** Needs a rule before launch, especially for accounts with credit or history.
- **Apple on iOS:** from memory, App Store rules require an option like Apple sign-in if an iOS app offers Google. Apple's developer program costs about $99 a year. Confirm both. If correct, Apple must be ready before the iOS release.
- **Going live with Google:** from memory, the Google project starts in testing mode, limited to 100 test users, and must be switched to production before launch. Confirm in the console.

## 11. What comes when
| Version | Work |
|---|---|
| `v0.2.0` | `people`, the identity table, rider ID, the `applicant` status, rider password storage, the OTP purpose, a separate service database login. Contract and mock updates, including `rider_not_approved`, and revoking only the chain. Session system. Customer Google and phone verification. Rider phone or ID sign-in, with OTP or password. Test OTPs until MSG91 and DLT are ready |
| `v0.3.0` | The rider application flow, with rider profiles |
| Before the iOS release | Apple sign-in |
| `v0.8.0` | Partner sign-in. `refresh_tokens`, `otp_requests` and `devices` are widened to allow the new user type |
| `v0.9.0` | Staff sign-in, roles and the authenticator. The same tables are widened |

## 12. Related documents
- Sign-in rules for apps: `contracts/README.md`
- Tables: `docs/engineering/endpoint_data_map.md`
- Discussion and history: Issue #25
