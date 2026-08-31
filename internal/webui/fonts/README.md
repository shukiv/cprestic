# Vendored typefaces

The interface is set in Fira Sans, with Fira Code for paths, cron
expressions and snapshot ids. Both are SIL Open Font License 1.1, so they
ship inside the plugin rather than being fetched from a font host: this
page runs as root inside WHM, and a privileged interface should not be
telling a third party when a root session is open, nor letting one serve
styling into it.

The files are the latin subsets Google Fonts builds, taken once:

    fira-sans-400.woff2  Fira Sans Regular   (Mozilla Foundation, Telefónica)
    fira-sans-500.woff2  Fira Sans Medium
    fira-sans-600.woff2  Fira Sans SemiBold
    fira-sans-700.woff2  Fira Sans Bold
    fira-code.woff2      Fira Code, variable 400-500 (Nikita Prokopov et al.)

OFL.txt is the licence both are published under. A browser that cannot
load them falls back to the system stack named in the stylesheet, which
is what every page did before they were vendored.
