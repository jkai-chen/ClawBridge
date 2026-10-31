import js from "@eslint/js";
import tseslint from "typescript-eslint";

// Untrusted text (Linear titles, messages, session notes) must reach the DOM
// only through textContent (GUI-DESIGN §7). These sinks parse markup, so they
// are banned outright; test/no-html-sinks.test.ts checks the same list.
const sinks = ["innerHTML", "outerHTML", "insertAdjacentHTML", "createContextualFragment", "srcdoc"];

export default tseslint.config(
  { ignores: ["dist/**", "node_modules/**", "coverage/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["src/**/*.ts"],
    rules: {
      "no-restricted-properties": [
        "error",
        ...sinks.map((property) => ({ property, message: `${property} parses markup; set text with textContent.` })),
        { object: "document", property: "write", message: "document.write parses markup." },
        { object: "document", property: "writeln", message: "document.writeln parses markup." },
      ],
      "no-restricted-syntax": [
        "error",
        { selector: "Literal[value=/^(innerHTML|outerHTML|insertAdjacentHTML|createContextualFragment)$/]", message: "A markup sink named as a string is still a markup sink." },
        { selector: "NewExpression[callee.name='DOMParser']", message: "DOMParser parses markup." },
      ],
      "no-eval": "error",
      "no-implied-eval": "error",
      "no-new-func": "error",
      "@typescript-eslint/no-explicit-any": "error",
    },
  },
);
