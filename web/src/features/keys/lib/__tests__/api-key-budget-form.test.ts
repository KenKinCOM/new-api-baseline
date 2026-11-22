/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import type { TFunction } from 'i18next'
import { describe, expect, test } from 'vitest'

import { parseQuotaFromDollars } from '@/lib/format'

import {
  getApiKeyFormDefaultValues,
  getApiKeyFormSchema,
  transformApiKeyToFormDefaults,
  transformFormDataToPayload,
} from '../api-key-form'
import type { ApiKey } from '../../types'

const t = ((key: string) => key) as TFunction

const baseApiKey: ApiKey = {
  id: 2,
  name: 'budgeted',
  key: 'sk-budget',
  status: 1,
  remain_quota: 0,
  used_quota: 0,
  unlimited_quota: true,
  expired_time: -1,
  created_time: 1,
  accessed_time: 0,
  group: 'default',
  auto_groups: null,
  cross_group_retry: false,
  model_limits_enabled: false,
  model_limits: '',
  allow_ips: '',
  allow_user_quota_query: true,
  budget_quota: 500000,
  budget_period: 'daily',
  budget_used: 12000,
  budget_window_start: 1700000000,
}

describe('API key periodic budget form', () => {
  test('sends no budget when the period is not enabled', () => {
    const payload = transformFormDataToPayload(getApiKeyFormDefaultValues(false))
    expect(payload.budget_quota).toBe(0)
    expect(payload.budget_period).toBe('')
  })

  test('converts a daily budget in currency units to quota units', () => {
    const payload = transformFormDataToPayload({
      ...getApiKeyFormDefaultValues(false),
      budget_period: 'daily',
      budget_quota_dollars: 5,
    })
    expect(payload.budget_period).toBe('daily')
    expect(payload.budget_quota).toBe(parseQuotaFromDollars(5))
  })

  test('restores the saved budget into the form defaults', () => {
    const values = transformApiKeyToFormDefaults(baseApiKey)
    expect(values.budget_period).toBe('daily')
    expect(values.budget_quota_dollars).toBeGreaterThan(0)
  })

  test('requires a positive amount once a period is selected', () => {
    const schema = getApiKeyFormSchema(t)
    const result = schema.safeParse({
      ...getApiKeyFormDefaultValues(false),
      name: 'budgeted',
      budget_period: 'monthly',
      budget_quota_dollars: 0,
    })
    expect(result.success).toBe(false)
    if (!result.success) {
      expect(result.error.issues.map((issue) => issue.path)).toContainEqual([
        'budget_quota_dollars',
      ])
    }
  })
})
