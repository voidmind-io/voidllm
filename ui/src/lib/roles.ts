import type { MeResponse } from '../hooks/useMe'

/** True when the user may view organization-wide data (org_admin or system_admin). */
export function isOrgAdmin(me: Pick<MeResponse, 'role' | 'is_system_admin'> | undefined): boolean {
  if (!me) return false
  return me.role === 'org_admin' || me.role === 'system_admin' || me.is_system_admin === true
}
