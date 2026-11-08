import sonarjs from "eslint-plugin-sonarjs";
import typescriptEslint from "typescript-eslint";

// The size limits for one function. Lines: the same limit the size check sets
// for Go, Python and shell, and every line of the function counts.
// Complexity: the number of decision points, where a switch counts once
// however many cases it has. Cognitive complexity: how hard the function is
// to follow (SonarQube's rule S3776), the same limit the size check sets for
// Go and Python.
const FUNCTION_LINES = 100;
const COMPLEXITY = 15;
const COGNITIVE = 15;

const sizeRules = (limit) => ({
    "max-lines-per-function": ["error", { max: limit.lines }],
    complexity: ["error", { max: limit.complexity, variant: "modified" }],
    "sonarjs/cognitive-complexity": ["error", limit.cognitive],
});

// Files that hold a function over a limit, each with the size of that
// function. ESLint sets a limit for a whole file, so the listed function
// cannot grow and no other function in the file can pass its size. To change
// a listed function, split it; then lower the number here, or remove the
// entry when the file meets the limits above. Do not add an entry or raise a
// number to make a change pass.
const OVER_THE_LIMIT = {
    // predictEdit
    "src/session/editPreview.ts": { lines: FUNCTION_LINES, complexity: 17, cognitive: COGNITIVE },
    // resolveToolChip
    "media/chat.js": { lines: FUNCTION_LINES, complexity: 16, cognitive: 24 },
};

export default [{
    files: ["**/*.ts"],
}, {
    plugins: {
        "@typescript-eslint": typescriptEslint.plugin,
        sonarjs,
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

        ...sizeRules({ lines: FUNCTION_LINES, complexity: COMPLEXITY, cognitive: COGNITIVE }),
    },
}, {
    // A test file holds its cases in one describe function, so its length
    // says nothing. The size check does not measure test length either.
    files: ["test/**"],
    rules: {
        "max-lines-per-function": "off",
    },
}, ...Object.entries(OVER_THE_LIMIT).map(([file, limit]) => ({
    files: [file],
    rules: sizeRules(limit),
}))];
