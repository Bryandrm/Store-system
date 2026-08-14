import { expect, test } from '@playwright/test'

import {
  countSales,
  login,
  resetDatabase,
  sql,
  stockFor,
  tapProduct,
  totalSoldCents,
} from './helpers'

test.beforeEach(() => {
  resetDatabase()
})

test('login survives a reload', async ({ page }) => {
  await login(page)

  await page.reload()

  // The session lives in IndexedDB, so a reload must not send the user back to
  // the login screen. A phone in a pocket reloads the tab constantly.
  await expect(page.getByRole('button', { name: 'Cobrar' })).toBeVisible()
  await expect(page.getByLabel('Usuario')).toHaveCount(0)
})

test('wrong credentials do not reveal whether the user exists', async ({ page }) => {
  await page.goto('/')
  await page.getByLabel('Usuario').fill('e2e')
  await page.getByLabel('Contraseña').fill('incorrecta')
  await page.getByRole('button', { name: 'Entrar' }).click()

  const withRealUser = await page.getByRole('alert').textContent()

  await page.getByLabel('Usuario').fill('nadie-con-este-nombre')
  await page.getByRole('button', { name: 'Entrar' }).click()

  await expect(page.getByRole('alert')).toHaveText(withRealUser!)
})

test('records a sale and shows it in the day view', async ({ page }) => {
  await login(page)

  // 2 x 0.50 + 1 x 0.25 = 1.25
  await tapProduct(page, 'Mani japones')
  await tapProduct(page, 'Mani japones')
  await tapProduct(page, 'Semillas')

  await expect(page.getByRole('status')).toHaveText('$1.25')

  await page.getByRole('button', { name: 'Cobrar' }).click()

  // Charging switches to the day view, where the sale must appear with its total.
  await expect(page.getByText('Ventas de hoy')).toBeVisible()
  await expect(page.getByText('$1.25').first()).toBeVisible()

  // And it must reach the server, not just the screen.
  await expect.poll(() => countSales(), { timeout: 15_000 }).toBe(1)
  expect(totalSoldCents()).toBe(125)
})

test('selling decrements derived stock', async ({ page }) => {
  await login(page)
  const before = stockFor('Mani japones')

  await tapProduct(page, 'Mani japones')
  await tapProduct(page, 'Mani japones')
  await page.getByRole('button', { name: 'Cobrar' }).click()

  await expect.poll(() => stockFor('Mani japones'), { timeout: 15_000 }).toBe(before - 2000)
})

test('the same product tapped twice becomes one line of two', async ({ page }) => {
  await login(page)

  await tapProduct(page, 'Mani japones')
  await tapProduct(page, 'Mani japones')
  await page.getByRole('button', { name: 'Cobrar' }).click()

  await expect.poll(() => countSales(), { timeout: 15_000 }).toBe(1)

  // One line with quantity two, not two lines of one.
  expect(Number.parseInt(sql('SELECT count(*) FROM sale_lines'), 10)).toBe(1)
  expect(Number.parseInt(sql('SELECT qty_milli FROM sale_lines'), 10)).toBe(2000)
})

test('an empty cart cannot be charged', async ({ page }) => {
  await login(page)

  await expect(page.getByRole('button', { name: 'Cobrar' })).toBeDisabled()
  expect(countSales()).toBe(0)
})

test('emptying the cart clears the total', async ({ page }) => {
  await login(page)

  await tapProduct(page, 'Churros')
  await expect(page.getByRole('status')).toHaveText('$1.00')

  await page.getByRole('button', { name: 'Vaciar' }).click()
  await expect(page.getByRole('status')).toHaveText('$0.00')
})
