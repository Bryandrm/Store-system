import { expect, test } from '@playwright/test'

import { countSales, login, resetDatabase, tapProduct } from './helpers'

/**
 * A phone in a pocket reloads the tab. Losing a half-rung sale in front of a
 * customer is the worst bug a point-of-sale app can have, so the cart is
 * persisted to IndexedDB on every change rather than held in memory.
 */

test.beforeEach(() => {
  resetDatabase()
})

test('the cart survives a reload', async ({ page }) => {
  await login(page)

  await tapProduct(page, 'Mani japones')
  await tapProduct(page, 'Mani japones')
  await tapProduct(page, 'Churros')
  await expect(page.getByRole('status')).toHaveText('$2.00')

  await page.reload()

  await expect(page.getByRole('status')).toHaveText('$2.00')
  // Scoped to the cart line, because the product name also appears on its grid
  // tile and an unscoped match resolves to two elements.
  await expect(page.getByRole('button', { name: /Mani japones2 ×/ })).toBeVisible()
  await expect(page.getByRole('button', { name: /Churros1 ×/ })).toBeVisible()

  // And it can still be charged afterwards: the cart is restored usable, not
  // merely displayed.
  await page.getByRole('button', { name: 'Cobrar' }).click()
  await expect.poll(() => countSales(), { timeout: 15_000 }).toBe(1)
})

test('the cart survives the app being closed and reopened', async ({ page, context }) => {
  await login(page)

  await tapProduct(page, 'Semillas')
  await expect(page.getByRole('status')).toHaveText('$0.25')

  await page.close()

  const reopened = await context.newPage()
  await reopened.goto('/')

  await expect(reopened.getByRole('status')).toHaveText('$0.25')
})

test('charging clears the cart, and the reload does not bring it back', async ({ page }) => {
  await login(page)

  await tapProduct(page, 'Churros')
  await page.getByRole('button', { name: 'Cobrar' }).click()
  await expect(page.getByText('Ventas de hoy')).toBeVisible()

  await page.reload()
  await page.getByRole('button', { name: 'Vender' }).click()

  // A cart that reappeared after being charged would be rung up twice.
  await expect(page.getByRole('status')).toHaveText('$0.00')
})

test('removing a line updates the total and persists', async ({ page }) => {
  await login(page)

  await tapProduct(page, 'Mani japones')
  await tapProduct(page, 'Churros')
  await expect(page.getByRole('status')).toHaveText('$1.50')

  await page.getByRole('button', { name: 'Quitar Churros' }).click()
  await expect(page.getByRole('status')).toHaveText('$0.50')

  await page.reload()
  await expect(page.getByRole('status')).toHaveText('$0.50')
})
