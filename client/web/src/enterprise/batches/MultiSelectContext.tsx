import React, { useCallback, useState } from 'react'

/**
 * The current selection state: either a set of IDs or "all", in which case all
 * possible IDs will be considered selected.
 *
 * Note that there is no special case for "visible": when all visible items are
 * selected and a new page is loaded, the expectation is that those new items
 * will not be selected by default.
 */
export type MultiSelectContextSelected = Set<string> | 'all'

export interface MultiSelectContextState {
    // State fields. These must not be mutated other than through the mutator
    // functions below, but may be read at any time.
    selected: MultiSelectContextSelected
    totalCount?: number
    visible: Set<string>

    // Convenience getters that abstract over the possible values of selected.
    areAllVisibleSelected: () => boolean
    isSelected: (id: string) => boolean

    // General state mutators to select and deselect items.
    deselectAll: () => void
    deselectVisible: () => void
    deselectSingle: (id: string) => void
    selectAll: () => void
    selectVisible: () => void
    selectSingle: (id: string) => void

    // Sets the total number of possible selections, if known.
    setTotalCount: (count?: number) => void

    // Sets the current set of visible IDs. This needs to happen in a single
    // call to avoid unnecessary re-renders: consumers are responsible for
    // aggregating the existing state from visible if required (for example, if
    // pagination is being performed by appending to the existing list in an
    // infinite scrolling style approach).
    setVisible: (ids: string[]) => void
}

// eslint-disable @typescript-eslint/no-unused-vars
const defaultState = (): MultiSelectContextState => ({
    selected: new Set(),
    totalCount: undefined,
    visible: new Set(),
    areAllVisibleSelected: () => false,
    isSelected: () => false,
    deselectAll: () => {},
    deselectVisible: () => {},
    deselectSingle: () => {},
    selectAll: () => {},
    selectVisible: () => {},
    selectSingle: () => {},
    setTotalCount: () => {},
    setVisible: () => {},
})
// eslint-enable @typescript-eslint/no-unused-vars

/**
 * MultiSelectContext is a context that tracks which checkboxes in a paginated
 * list have been selected, providing options to select visible and select all.
 * Options are tracked by opaque string IDs.
 *
 * Use MultiSelectContextProvider to instantiate a MultiSelectContext: this will
 * set up the appropriate internal state.
 */
export const MultiSelectContext = React.createContext<MultiSelectContextState>(defaultState())

/**
 * MultiSelectContextProvider returns a pre-canned <MultiSelectContext.Provider>
 * that has the correct state handling for normal use, including providing the
 * various callbacks that are used by consumers.
 */
export const MultiSelectContextProvider: React.FunctionComponent<{
    // These props are only for testing purposes.
    initialSelected?: MultiSelectContextSelected | string[]
    initialTotalCount?: number
    initialVisible?: string[]
}> = ({ children, initialSelected, initialTotalCount, initialVisible }) => {
    // Set up state and callbacks for the visible items.
    const [visible, setVisibleInternal] = useState<Set<string>>(new Set(initialVisible ?? []))
    const setVisible = useCallback((ids: string[]) => {
        setVisibleInternal(new Set(ids))
    }, [])

    // Now for selected items.
    const [selected, setSelected] = useState<MultiSelectContextSelected>(
        initialSelected === 'all' ? 'all' : new Set(initialSelected)
    )
    const selectAll = useCallback(() => setSelected('all'), [setSelected])
    const deselectAll = useCallback(() => setSelected(new Set()), [setSelected])

    // Total count handling.
    const [totalCount, setTotalCountInternal] = useState<number | undefined>(initialTotalCount)
    const setTotalCount = useCallback((totalCount?: number) => {
        setTotalCountInternal(totalCount)
    }, [])

    // Callbacks to select and deselect items.
    const selectVisible = useCallback(() => {
        if (selected === 'all') {
            // If all items are currently selected, we're going to switch to
            // only selecting the visible items.
            setSelected(new Set([...visible]))
        } else {
            // Otherwise, we can merge the visible items with any previously
            // selected items.
            setSelected(new Set([...visible, ...selected]))
        }
    }, [selected, visible])

    const deselectVisible = useCallback(() => {
        if (selected === 'all') {
            // If all items are currently selected, there isn't a sensible way
            // to say "except for this specific subset" within the current data
            // model, so we'll just interpret that as "deselect them all".
            setSelected(new Set())
        } else {
            // Otherwise, we remove the items and create a new set.
            setSelected(new Set([...selected].filter(id => !visible.has(id))))
        }
    }, [selected, visible])

    const selectSingle = useCallback(
        (id: string) => {
            if (selected === 'all') {
                // If all items are selected, then... we don't need to do
                // anything, because this item is selected by definition.
                // (Although it's probably a bug in the UI component.)
                return
            }

            const updated = new Set(selected)
            updated.add(id)

            setSelected(updated)
        },
        [selected]
    )

    const deselectSingle = useCallback(
        (id: string) => {
            let updated: Set<string> | undefined
            if (selected === 'all') {
                // If all items are currently selected, there isn't a sensible
                // way to say "except for this specific subset" within the
                // current data model, so we'll just interpret that as "select
                // visible, then deselect this particular item".
                updated = new Set(visible)
            } else {
                updated = new Set(selected)
            }

            updated.delete(id)
            setSelected(updated)
        },
        [selected, visible]
    )

    // Helper functions to access the current selection state.
    const areAllVisibleSelected = useCallback(() => {
        if (selected === 'all') {
            return true
        }

        for (const id of visible) {
            if (!selected.has(id)) {
                return false
            }
        }

        return true
    }, [selected, visible])

    const isSelected = useCallback((id: string) => selected === 'all' || selected.has(id), [selected])

    return (
        <MultiSelectContext.Provider
            value={{
                selected,
                totalCount,
                visible,
                areAllVisibleSelected,
                isSelected,
                deselectAll,
                deselectVisible,
                deselectSingle,
                selectAll,
                selectVisible,
                selectSingle,
                setTotalCount,
                setVisible,
            }}
        >
            {children}
        </MultiSelectContext.Provider>
    )
}
