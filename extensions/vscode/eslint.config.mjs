import typescriptEslint from "typescript-eslint";

// The size limits for one function. Lines: the same limit the size check sets
// for Go and Python, and every line of the function counts. Complexity: the
// number of decision points, where a switch counts once however many cases
// it has.
const FUNCTION_LINES = 100;
const COMPLEXITY = 15;

const sizeRules = (lines, complexity) => ({
    "max-lines-per-function": ["error", { max: lines }],
    complexity: ["error", { max: complexity, variant: "modified" }],
});

// Files that hold a function over a limit, each with the size of that
// function. ESLint sets a limit for a whole file, so the listed function
// cannot grow and no other function in the file can pass its size. To change
// a listed function, split it; then lower the number here, or remove the
// entry when the file meets the limits above. Do not add an entry or raise a
// number to make a change pass.
const OVER_THE_LIMIT = {
    // ChatViewProvider.dispatch
    "src/ui/chatView.ts": { lines: 188, complexity: 33 },
    // predictEdit
    "src/session/editPreview.ts": { lines: FUNCTION_LINES, complexity: 17 },
};

export default [{
    files: ["**/*.ts"],
}, {
    plugins: {
        "@typescript-eslint": typescriptEslint.plugin,
    },

    languageOptions: {
        parser: typescriptEslint.parser,
        ecmaVersion: 2022,
        sourceType: "module",
    },

    rules: {
        "@typescript-eslint/naming-convention": ["warn", {
            selector: "import",
            format: ["camelCase", "PascalCase"],
        }],

        curly: "warn",
        eqeqeq: "warn",
        "no-throw-literal": "warn",
        semi: "warn",

        ...sizeRules(FUNCTION_LINES, COMPLEXITY),
    },
}, ...Object.entries(OVER_THE_LIMIT).map(([file, limit]) => ({
    files: [file],
    rules: sizeRules(limit.lines, limit.complexity),
}))];
