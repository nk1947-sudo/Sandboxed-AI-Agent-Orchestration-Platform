/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      fontFamily: {
        mono: ['"JetBrains Mono"', "ui-monospace", "monospace"],
      },
      colors: {
        surface: {
          900: "#0d1117",
          800: "#161b22",
          700: "#21262d",
          600: "#30363d",
        },
      },
    },
  },
  plugins: [],
};
