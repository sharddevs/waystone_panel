// SPDX-License-Identifier: AGPL-3.0-only
import js from '@eslint/js'
import tseslint from 'typescript-eslint'
import reactHooks from 'eslint-plugin-react-hooks'

export default tseslint.config(
  { ignores: ['node_modules/'] },
  js.configs.recommended,
  ...tseslint.configs.strict,
  {
    files: ['src/**/*.{ts,tsx}'],
    plugins: { 'react-hooks': reactHooks },
    rules: {
      ...reactHooks.configs.recommended.rules,
      // Components use only CSS classes and tokens: a style attribute is
      // blocked by the CSP (style-src 'self') and bypasses the tokens.
      'no-restricted-syntax': [
        'error',
        { selector: "JSXAttribute[name.name='style']", message: 'No style={}: use a class and var(--token) (CSP style-src \'self\').' },
        { selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']", message: 'No dangerouslySetInnerHTML.' },
      ],
    },
  },
  {
    // The app runs in the browser: Node's modules are for tests only.
    files: ['src/**/*.{ts,tsx}'],
    ignores: ['src/**/*.test.{ts,tsx}', 'src/test/**'],
    rules: {
      'no-restricted-imports': ['error', { patterns: [{ group: ['node:*'], message: 'Node modules are for tests only.' }] }],
    },
  },
)
