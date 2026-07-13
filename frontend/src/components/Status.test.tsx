// @vitest-environment jsdom
import '@testing-library/jest-dom/vitest'
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Status } from './Status'
describe('Status',()=>{it('renders normalized state text and state class',()=>{render(<Status value="plan_failed"/>);const status=screen.getByText('规划失败');expect(status).toHaveClass('status-plan_failed')})})
