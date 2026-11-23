import type { APIKeyResponse } from '../hooks/useAPIKeys'
import type { MeResponse } from '../hooks/useMe'
import { isOrgAdmin } from './roles'

type KeyActor = Pick<MeResponse, 'id' | 'role' | 'is_system_admin'>
type KeySubject = Pick<APIKeyResponse, 'user_id' | 'team_id' | 'key_type'>

function isOwnKey(me: KeyActor, key: KeySubject): boolean {
  return key.user_id !== null && key.user_id === me.id
}

// The backend also requires a team_admin's own team for team keys. The /me
// response carries no team membership, so the UI cannot check it and shows the
// action for any team key; the backend's answer still surfaces as a toast.
function isTeamKeyForTeamAdmin(me: KeyActor, key: KeySubject): boolean {
  return me.role === 'team_admin' && key.key_type === 'team_key' && !!key.team_id
}

/** Whether the user should be offered the edit action for the key. */
export function canEditKey(me: KeyActor | undefined, key: KeySubject): boolean {
  if (!me) return false
  if (isOrgAdmin(me)) return true
  return isOwnKey(me, key) || isTeamKeyForTeamAdmin(me, key)
}

/** Whether the user should be offered the rotate action for the key. */
export function canRotateKey(me: KeyActor | undefined, key: KeySubject): boolean {
  if (!me) return false
  if (isOrgAdmin(me)) return true
  return isOwnKey(me, key) || isTeamKeyForTeamAdmin(me, key)
}

/** Whether the user should be offered the revoke action. Below org_admin only own keys. */
export function canRevokeKey(me: KeyActor | undefined, key: KeySubject): boolean {
  if (!me) return false
  if (isOrgAdmin(me)) return true
  return isOwnKey(me, key)
}
