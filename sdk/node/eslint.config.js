// ESLint flat config. `jsdoc/require-jsdoc` on exported symbols enforces the
// documentation standard (docs/plan/documentation.md §2) for the SDK, the same
// way revive's `exported` rule does for Go and the web UI's config does there.
import js from "@eslint/js";
import jsdoc from "eslint-plugin-jsdoc";
import globals from "globals";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["node_modules", "coverage", "dist"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["**/*.{ts,js,mjs}"],
    languageOptions: { globals: globals.node },
    plugins: { jsdoc },
    rules: {
      "jsdoc/require-jsdoc": [
        "error",
        {
          publicOnly: true,
          require: { FunctionDeclaration: true, ArrowFunctionExpression: true, ClassDeclaration: true, MethodDefinition: true },
          contexts: ["TSInterfaceDeclaration", "TSTypeAliasDeclaration"],
        },
      ],
    },
  },
  {
    files: ["test/**"],
    rules: { "jsdoc/require-jsdoc": "off" },
  },
);
