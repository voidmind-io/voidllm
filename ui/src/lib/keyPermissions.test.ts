import { describe, it, expect } from 'vitest'
import { canEditKey, canRotateKey, canRevokeKey } from './keyPermissions'

const member = { id: 'u1', role: 'member', is_system_admin: false }
const teamAdmin = { id: 'u2', role: 'team_admin', is_system_admin: false }
const orgAdmin = { id: 'u3', role: 'org_admin', is_system_admin: false }
const sysAdmin = { id: 'u4', role: 'system_admin', is_system_admin: true }

const ownKey = { user_id: 'u1', team_id: null, key_type: 'user_key' }
const otherKey = { user_id: 'someone', team_id: null, key_type: 'user_key' }
const teamKey = { user_id: null, team_id: 't1', key_type: 'team_key' }
const saKey = { user_id: null, team_id: null, key_type: 'sa_key' }

describe('keyPermissions', () => {
  it('denies everything while me is unknown', () => {
    expect(canEditKey(undefined, ownKey)).toBe(false)
    expect(canRotateKey(undefined, ownKey)).toBe(false)
    expect(canRevokeKey(undefined, ownKey)).toBe(false)
  })

  it('allows org and system admins on any key', () => {
    for (const me of [orgAdmin, sysAdmin]) {
      for (const key of [ownKey, otherKey, teamKey, saKey]) {
        expect(canEditKey(me, key)).toBe(true)
        expect(canRotateKey(me, key)).toBe(true)
        expect(canRevokeKey(me, key)).toBe(true)
      }
    }
  })

  it('limits members to their own keys', () => {
    expect(canEditKey(member, ownKey)).toBe(true)
    expect(canRotateKey(member, ownKey)).toBe(true)
    expect(canRevokeKey(member, ownKey)).toBe(true)
    for (const key of [otherKey, teamKey, saKey]) {
      expect(canEditKey(member, key)).toBe(false)
      expect(canRotateKey(member, key)).toBe(false)
      expect(canRevokeKey(member, key)).toBe(false)
    }
  })

  it('lets team admins edit and rotate team keys but not revoke them', () => {
    expect(canEditKey(teamAdmin, teamKey)).toBe(true)
    expect(canRotateKey(teamAdmin, teamKey)).toBe(true)
    expect(canRevokeKey(teamAdmin, teamKey)).toBe(false)
    expect(canEditKey(teamAdmin, otherKey)).toBe(false)
    expect(canRotateKey(teamAdmin, saKey)).toBe(false)
  })

  it('lets team admins manage their own keys fully', () => {
    const own = { user_id: 'u2', team_id: null, key_type: 'user_key' }
    expect(canEditKey(teamAdmin, own)).toBe(true)
    expect(canRevokeKey(teamAdmin, own)).toBe(true)
  })
})
