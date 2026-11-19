# gitdm test fixtures

`developers_affiliations1.txt`, `developers_affiliations2.txt` and `aliases`
are small, unmodified excerpts of the public files of the same names in
[cncf/gitdm](https://github.com/cncf/gitdm) (repository root and
`src/cncf-config/aliases`), taken at commit
`edb90d7a8fd0238d49701582adf19b22e3e65430` (2026-11-19).

They are kept minimal and are used only as parser and matching fixtures.
cncf/gitdm's code is distributed under the GNU General Public License,
version 2 (see `src/COPYING` in that repository); its data files carry no
separate licence statement. Ownership of the data stays with cncf/gitdm and
the people listed; to change or remove an entry, follow gitdm's own process
(`FORBIDDEN_DATA.md` in cncf/gitdm), and colony will drop it from these
fixtures on request.

The entries were chosen to cover the format: a single current affiliation,
long dated histories, an organisation name that itself contains " from "
(`Nuage Networks from Nokia`), the same login listed twice with different
case (`AdoHe` and `adohe`), and a real alias line.
