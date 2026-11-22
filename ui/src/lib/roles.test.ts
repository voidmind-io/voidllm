import { describe, it, expect } from 'vitest'
import { isOrgAdmin } from './roles'

describe('isOrgAdmin', () => {
  it('accepts org_admin, system_admin role and is_system_admin flag', () => {
    expect(isOrgAdmin({ role: 'org_admin', is_system_admin: false })).toBe(true)
    expect(isOrgAdmin({ role: 'system_admin', is_system_admin: false })).toBe(true)
    expect(isOrgAdmin({ role: 'member', is_system_admin: true })).toBe(true)
  })

  it('rejects members and undefined', () => {
    expect(isOrgAdmin({ role: 'member', is_system_admin: false })).toBe(false)
    expect(isOrgAdmin({ role: 'team_admin', is_system_admin: false })).toBe(false)
    expect(isOrgAdmin(undefined)).toBe(false)
  })
})
